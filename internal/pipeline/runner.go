package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/config"
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
}

// Runner coordena a execução sequencial de etapas de pipeline com cancelamento e auditoria.
type Runner struct {
	store     QueueStore
	registry  *providers.Registry
	watchers  map[string]config.WatcherConfig
	pipelines map[string]config.Pipeline
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

	// 4. Execução Sequencial dos Steps
	for _, step := range pipe.Steps {
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

		basePath := watcher.ResolvedPath
		if basePath == "" {
			basePath = watcher.Path
		}

		stepCtx := &providers.StepContext{
			Context:      execCtx,
			WatcherName:  watcher.Name,
			BasePath:     basePath,
			ChangedFiles: job.PayloadFiles,
			StepParams:   step.Params,
			LastOutput:   lastOutput,
			Timestamp:    time.Now(),
		}

		res, execErr := action.Execute(stepCtx)
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

	// 5. Tratamento de Falha
	if stepErr != nil {
		category := ClassifyError(stepErr, stepRes)
		errMsg := stepErr.Error()

		status := "FAILED"
		if category == CategoryConflict {
			status = "BLOCKED"
		}

		if r.store != nil {
			_ = r.store.RecordPipelineRun(&queue.PipelineRun{
				ID:           runID,
				JobID:        job.ID,
				WatcherID:    job.WatcherID,
				PipelineName: job.PipelineName,
				Status:       status,
				DurationMs:   durationMs,
				ErrorStep:    failedStep,
				ErrorDetails: errMsg,
			})

			switch category {
			case CategoryConflict:
				_ = r.store.MarkJobBlocked(job.ID, errMsg)
				_ = r.store.UpdateWatcherStatus(job.WatcherID, queue.WatcherConflictHalted, errMsg)
			case CategoryTransient:
				maxRetries := job.MaxRetries
				if maxRetries <= 0 {
					maxRetries = 5
				}
				canRetry := (job.RetryCount + 1) < maxRetries
				backoff := CalculateBackoff(job.RetryCount)
				_ = r.store.MarkJobFailed(job.ID, errMsg, canRetry, backoff)
				_ = r.store.UpdateWatcherStatus(job.WatcherID, queue.WatcherDegraded, errMsg)
			case CategoryFatal:
				_ = r.store.MarkJobFailed(job.ID, errMsg, false, 0)
				_ = r.store.UpdateWatcherSyncTime(job.WatcherID, false, errMsg)
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
			StepResults: jobCtx.StepResults,
		}, stepErr
	}

	// 6. Sucesso Completo
	if r.store != nil {
		_ = r.store.RecordPipelineRun(&queue.PipelineRun{
			ID:           runID,
			JobID:        job.ID,
			WatcherID:    job.WatcherID,
			PipelineName: job.PipelineName,
			Status:       "SUCCESS",
			DurationMs:   durationMs,
		})
		_ = r.store.MarkJobCompleted(job.ID)
		_ = r.store.UpdateWatcherSyncTime(job.WatcherID, true, "")
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

	if r.store != nil {
		_ = r.store.RecordPipelineRun(&queue.PipelineRun{
			ID:           runID,
			JobID:        job.ID,
			WatcherID:    job.WatcherID,
			PipelineName: job.PipelineName,
			Status:       "FAILED",
			DurationMs:   durationMs,
			ErrorStep:    failedStep,
			ErrorDetails: errMsg,
		})
		_ = r.store.MarkJobFailed(job.ID, errMsg, false, 0)
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
