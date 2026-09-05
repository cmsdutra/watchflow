package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
)

// QueueStore abstrai as operações de persistência necessárias para o Runner.
type QueueStore interface {
	RecordPipelineRun(run *queue.PipelineRun) error
	MarkJobCompleted(jobID string) error
	MarkJobFailed(jobID string, lastError string, canRetry bool, backoff time.Duration) error
	MarkJobBlocked(jobID string, reason string) error
	UpdateWatcherStatus(id string, status queue.WatcherStatus, lastErr string) error
	UpdateWatcherSyncTime(id string, success bool, lastErr string) error
	GetWatcher(id string) (*queue.WatcherRecord, error)
	GetWatcherByName(name string) (*queue.WatcherRecord, error)
}

// RunResult sintetiza o resultado completo da execução de um pipeline.
type RunResult struct {
	RunID       string
	JobID       string
	WatcherID   string
	Pipeline    string
	Success     bool
	Duration    time.Duration
	ErrorStep   string
	Error       error
	Category    ErrorCategory
	StepResults map[string]*providers.StepResult

	// WillRetry indica que o job foi reagendado e terá nova tentativa.
	// Permite ao chamador distinguir uma falha transitória de uma definitiva
	// (ex.: para não alertar o usuário a cada retry).
	WillRetry bool
}

// Runner coordena a execução sequencial de etapas de pipeline com cancelamento e auditoria.
type Runner struct {
	store     QueueStore
	registry  *providers.Registry
	watchers  map[string]config.WatcherConfig
	pipelines map[string]config.Pipeline
	log       *slog.Logger
	mu        sync.RWMutex
}

// NewRunner cria uma nova instância do Runner de pipelines.
func NewRunner(store QueueStore, registry *providers.Registry, cfg *config.Config) *Runner {
	if registry == nil {
		registry = providers.DefaultRegistry
	}

	r := &Runner{
		store:     store,
		registry:  registry,
		watchers:  make(map[string]config.WatcherConfig),
		pipelines: make(map[string]config.Pipeline),
		log:       logger.For("pipeline"),
	}

	if cfg != nil {
		r.UpdateConfig(cfg)
	}

	return r
}

// UpdateConfig atualiza as configurações em memória de watchers e pipelines de forma thread-safe.
func (r *Runner) UpdateConfig(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.watchers = make(map[string]config.WatcherConfig)
	for _, w := range cfg.Watchers {
		r.watchers[w.Name] = w
	}

	r.pipelines = make(map[string]config.Pipeline)
	for name, p := range cfg.Pipelines {
		r.pipelines[name] = p
	}
}

// RegisterWatcher adiciona ou substitui manualmente uma configuração de watcher no Runner.
func (r *Runner) RegisterWatcher(w config.WatcherConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchers[w.Name] = w
}

// RegisterPipeline adiciona ou substitui um pipeline no catálogo do Runner.
func (r *Runner) RegisterPipeline(name string, p config.Pipeline) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pipelines[name] = p
}

// ExecuteJob executa sequencialmente todas as etapas do pipeline associado a um Job.
func (r *Runner) ExecuteJob(ctx context.Context, job *queue.Job) (*RunResult, error) {
	if job == nil {
		return nil, fmt.Errorf("job não pode ser nulo")
	}

	runID := fmt.Sprintf("run_%d_%s", time.Now().UnixNano(), job.ID)
	startTime := time.Now()

	runLog := r.log.With(
		slog.String("run_id", runID),
		slog.String("job_id", job.ID),
		slog.String("watcher", job.WatcherID),
		slog.String("pipeline", job.PipelineName))

	// 1. Resolução do Watcher
	watcher, err := r.resolveWatcher(job.WatcherID)
	if err != nil {
		return r.failRunImmediately(runID, job, "", err, CategoryFatal, startTime)
	}

	// 2. Resolução do Pipeline
	pipe, ok := r.getPipeline(job.PipelineName)
	if !ok {
		err := fmt.Errorf("pipeline '%s' não encontrado nas configurações", job.PipelineName)
		return r.failRunImmediately(runID, job, "", err, CategoryFatal, startTime)
	}

	// 3. Delimitação do Timeout de Execução
	timeout := pipe.TimeoutDuration
	if timeout <= 0 && pipe.Timeout != "" {
		if parsed, parseErr := time.ParseDuration(pipe.Timeout); parseErr == nil {
			timeout = parsed
		}
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	jobCtx := NewJobContext(execCtx, cancel, job, watcher, &pipe)

	var (
		lastOutput string
		failedStep string
		stepErr    error
		stepRes    *providers.StepResult
	)

	basePath := watcher.ResolvedPath
	if basePath == "" {
		basePath = watcher.Path
	}

	// 4. Trava exclusiva da árvore de trabalho por TODA a duração do pipeline.
	// Travar por action deixaria brechas entre steps nas quais um pipeline
	// concorrente sobre o mesmo repositório poderia intercalar (ex.: o commit de
	// um levaria o stage do outro), violando a garantia do AGENTS.md §3.3.
	repoLockHeld := false
	if basePath != "" {
		unlockRepo, lockErr := locking.DefaultRepoLocker.LockContext(execCtx, basePath)
		if lockErr != nil {
			failedStep = "repo.lock"
			stepErr = fmt.Errorf("não foi possível adquirir a trava exclusiva do repositório '%s': %w", basePath, lockErr)
			stepRes = &providers.StepResult{TransientErr: true}
		} else {
			defer unlockRepo()
			repoLockHeld = true
		}
	}

	// 5. Execução Sequencial dos Steps
	for _, step := range pipe.Steps {
		// Falha na aquisição da trava aborta antes de qualquer operação em disco
		if stepErr != nil {
			break
		}

		// Checa se o contexto expirou antes de invocar o step
		if err := execCtx.Err(); err != nil {
			failedStep = step.Action
			stepErr = fmt.Errorf("pipeline cancelado ou atingiu timeout de %v: %w", timeout, err)
			break
		}

		action, exists := r.registry.Get(step.Action)
		if !exists {
			failedStep = step.Action
			stepErr = fmt.Errorf("ação '%s' não registrada no catálogo de providers", step.Action)
			break
		}

		if err := action.Validate(step.Params); err != nil {
			failedStep = step.Action
			stepErr = fmt.Errorf("validação dos parâmetros da ação '%s' falhou: %w", step.Action, err)
			break
		}

		stepCtx := &providers.StepContext{
			Context:      execCtx,
			WatcherName:  watcher.Name,
			BasePath:     basePath,
			ChangedFiles: job.PayloadFiles,
			StepParams:   step.Params,
			LastOutput:   lastOutput,
			Timestamp:    time.Now(),
			RepoLockHeld: repoLockHeld,
		}

		stepStart := time.Now()
		res, execErr := action.Execute(stepCtx)
		stepLog := runLog.With(slog.String("step", step.Action), slog.Duration("duracao", time.Since(stepStart)))

		switch {
		case execErr != nil:
			stepLog.Warn("step falhou", slog.Any("error", execErr))
		case res != nil && res.Skipped:
			stepLog.Debug("step pulado", slog.String("motivo", res.Output))
		case res != nil && res.Success:
			stepLog.Debug("step concluído", slog.String("saida", res.Output))
		}

		if execErr != nil || (res != nil && !res.Success && !res.Skipped) {
			failedStep = step.Action
			stepRes = res
			if execErr != nil {
				stepErr = execErr
			} else if res != nil && res.ErrorMessage != "" {
				stepErr = fmt.Errorf("%s", res.ErrorMessage)
			} else {
				stepErr = fmt.Errorf("ação '%s' falhou sem detalhes de erro", step.Action)
			}
			// Interrupção imediata da cadeia — passos seguintes NÃO são executados
			break
		}

		if res != nil {
			jobCtx.RecordStep(step.Action, res)
			if res.Output != "" {
				lastOutput = res.Output
			}
		}
	}

	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	// 6. Tratamento de Falha
	if stepErr != nil {
		category := ClassifyError(stepErr, stepRes)
		errMsg := stepErr.Error()

		status := "FAILED"
		if category == CategoryConflict {
			status = "BLOCKED"
		}

		willRetry := false
		if category == CategoryTransient {
			maxRetries := job.MaxRetries
			if maxRetries <= 0 {
				maxRetries = 5
			}
			willRetry = (job.RetryCount + 1) < maxRetries
		}

		switch category {
		case CategoryConflict:
			runLog.Error("CONFLITO DE MERGE: watcher interrompido até intervenção manual",
				slog.String("step", failedStep), slog.String("detalhe", errMsg))
		case CategoryTransient:
			runLog.Warn("falha transitória; job será retentado",
				slog.String("step", failedStep), slog.String("detalhe", errMsg))
		default:
			runLog.Error("falha fatal do pipeline",
				slog.String("step", failedStep), slog.String("detalhe", errMsg))
		}

		if r.store != nil {
			if err := r.store.RecordPipelineRun(&queue.PipelineRun{
				ID:           runID,
				JobID:        job.ID,
				WatcherID:    job.WatcherID,
				PipelineName: job.PipelineName,
				Status:       status,
				DurationMs:   durationMs,
				ErrorStep:    failedStep,
				ErrorDetails: errMsg,
			}); err != nil {
				runLog.Error("falha ao registrar auditoria da execução", slog.Any("error", err))
			}

			switch category {
			case CategoryConflict:
				if err := r.store.MarkJobBlocked(job.ID, errMsg); err != nil {
					runLog.Error("falha ao marcar job como BLOCKED", slog.Any("error", err))
				}
				if err := r.store.UpdateWatcherStatus(job.WatcherID, queue.WatcherConflictHalted, errMsg); err != nil {
					runLog.Error("falha ao marcar watcher como CONFLICT_HALTED", slog.Any("error", err))
				}
			case CategoryTransient:
				canRetry := willRetry
				profile := ClassifyBackoff(stepErr, stepRes)
				backoff := BackoffFor(profile, job.RetryCount)
				runLog.Info("reagendando job",
					slog.Bool("pode_retentar", canRetry),
					slog.String("perfil_espera", profile.String()),
					slog.Duration("backoff", backoff))
				if err := r.store.MarkJobFailed(job.ID, errMsg, canRetry, backoff); err != nil {
					runLog.Error("falha ao reagendar job para retry", slog.Any("error", err))
				}
				if err := r.store.UpdateWatcherStatus(job.WatcherID, queue.WatcherDegraded, errMsg); err != nil {
					runLog.Error("falha ao marcar watcher como DEGRADED", slog.Any("error", err))
				}
			case CategoryFatal:
				if err := r.store.MarkJobFailed(job.ID, errMsg, false, 0); err != nil {
					runLog.Error("falha ao marcar job como FAILED", slog.Any("error", err))
				}
				if err := r.store.UpdateWatcherSyncTime(job.WatcherID, false, errMsg); err != nil {
					runLog.Error("falha ao registrar horário da última falha", slog.Any("error", err))
				}
			}
		}

		return &RunResult{
			RunID:       runID,
			JobID:       job.ID,
			WatcherID:   job.WatcherID,
			Pipeline:    job.PipelineName,
			Success:     false,
			Duration:    duration,
			ErrorStep:   failedStep,
			Error:       stepErr,
			Category:    category,
			WillRetry:   willRetry,
			StepResults: jobCtx.StepResults,
		}, stepErr
	}

	// 7. Sucesso Completo
	runLog.Info("pipeline concluído", slog.Duration("duracao", duration), slog.Int("steps", len(pipe.Steps)))

	if r.store != nil {
		if err := r.store.RecordPipelineRun(&queue.PipelineRun{
			ID:           runID,
			JobID:        job.ID,
			WatcherID:    job.WatcherID,
			PipelineName: job.PipelineName,
			Status:       "SUCCESS",
			DurationMs:   durationMs,
		}); err != nil {
			runLog.Error("falha ao registrar auditoria da execução bem-sucedida", slog.Any("error", err))
		}
		if err := r.store.MarkJobCompleted(job.ID); err != nil {
			// Job ficaria preso em RUNNING e seria reprocessado no próximo boot
			runLog.Error("falha ao marcar job como COMPLETED", slog.Any("error", err))
		}
		if err := r.store.UpdateWatcherSyncTime(job.WatcherID, true, ""); err != nil {
			runLog.Error("falha ao registrar horário do último sucesso", slog.Any("error", err))
		}
	}

	return &RunResult{
		RunID:       runID,
		JobID:       job.ID,
		WatcherID:   job.WatcherID,
		Pipeline:    job.PipelineName,
		Success:     true,
		Duration:    duration,
		StepResults: jobCtx.StepResults,
	}, nil
}

func (r *Runner) resolveWatcher(watcherID string) (*config.WatcherConfig, error) {
	r.mu.RLock()
	w, exists := r.watchers[watcherID]
	r.mu.RUnlock()
	if exists {
		return &w, nil
	}

	// Tenta fallback pelo store SQLite se disponível
	if r.store != nil {
		rec, err := r.store.GetWatcher(watcherID)
		if err == nil && rec != nil {
			return &config.WatcherConfig{
				Name:         rec.Name,
				Path:         rec.Path,
				ResolvedPath: rec.Path,
			}, nil
		}

		recByName, errByName := r.store.GetWatcherByName(watcherID)
		if errByName == nil && recByName != nil {
			return &config.WatcherConfig{
				Name:         recByName.Name,
				Path:         recByName.Path,
				ResolvedPath: recByName.Path,
			}, nil
		}
	}

	return nil, fmt.Errorf("watcher '%s' não encontrado nas configurações", watcherID)
}

func (r *Runner) getPipeline(name string) (config.Pipeline, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pipelines[name]
	return p, ok
}

func (r *Runner) failRunImmediately(
	runID string,
	job *queue.Job,
	failedStep string,
	err error,
	cat ErrorCategory,
	startTime time.Time,
) (*RunResult, error) {
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()
	errMsg := err.Error()

	r.log.Error("job abortado antes da execução dos steps",
		slog.String("run_id", runID),
		slog.String("job_id", job.ID),
		slog.String("watcher", job.WatcherID),
		slog.String("pipeline", job.PipelineName),
		slog.String("detalhe", errMsg))

	if r.store != nil {
		if recErr := r.store.RecordPipelineRun(&queue.PipelineRun{
			ID:           runID,
			JobID:        job.ID,
			WatcherID:    job.WatcherID,
			PipelineName: job.PipelineName,
			Status:       "FAILED",
			DurationMs:   durationMs,
			ErrorStep:    failedStep,
			ErrorDetails: errMsg,
		}); recErr != nil {
			r.log.Error("falha ao registrar auditoria da execução", slog.Any("error", recErr))
		}
		if markErr := r.store.MarkJobFailed(job.ID, errMsg, false, 0); markErr != nil {
			r.log.Error("falha ao marcar job como FAILED", slog.Any("error", markErr))
		}
	}

	return &RunResult{
		RunID:     runID,
		JobID:     job.ID,
		WatcherID: job.WatcherID,
		Pipeline:  job.PipelineName,
		Success:   false,
		Duration:  duration,
		ErrorStep: failedStep,
		Error:     err,
		Category:  cat,
	}, err
}
