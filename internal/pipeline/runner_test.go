package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
)

// mockAction implementa a interface providers.ActionProvider para testes controlados.
type mockAction struct {
	name        string
	execCount   int32
	executeFunc func(ctx *providers.StepContext) (*providers.StepResult, error)
}

func (m *mockAction) Name() string {
	return m.name
}

func (m *mockAction) Validate(params map[string]interface{}) error {
	if val, ok := params["fail_validation"]; ok && val.(bool) {
		return errors.New("validação mock rejeitada")
	}
	return nil
}

func (m *mockAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	atomic.AddInt32(&m.execCount, 1)
	if m.executeFunc != nil {
		return m.executeFunc(ctx)
	}
	return &providers.StepResult{
		Success: true,
		Output:  fmt.Sprintf("output from %s", m.name),
	}, nil
}

func (m *mockAction) Count() int {
	return int(atomic.LoadInt32(&m.execCount))
}

func setupTestStore(t *testing.T) *queue.Store {
	t.Helper()
	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao inicializar SQLite em memória: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	err = store.RegisterWatcher(&queue.WatcherRecord{
		ID:     "vault-notes",
		Name:   "vault-notes",
		Path:   "/tmp/test-vault",
		Status: queue.WatcherHealthy,
	})
	if err != nil {
		t.Fatalf("falha ao registrar watcher de teste: %v", err)
	}

	return store
}

func TestPipelineRunner_Success(t *testing.T) {
	store := setupTestStore(t)
	reg := providers.NewRegistry()

	step1 := &mockAction{name: "step.one"}
	step2 := &mockAction{name: "step.two"}

	if err := reg.Register(step1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(step2); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "vault-notes", Path: "/tmp/test-vault"},
		},
		Pipelines: map[string]config.Pipeline{
			"sync_pipe": {
				Timeout: "5s",
				Steps: []config.Step{
					{Action: "step.one"},
					{Action: "step.two"},
				},
			},
		},
	}

	runner := pipeline.NewRunner(store, reg, cfg)

	job := &queue.Job{
		ID:           "job-100",
		WatcherID:    "vault-notes",
		PipelineName: "sync_pipe",
		PayloadFiles: []string{"note1.md", "note2.md"},
		MaxRetries:   3,
	}
	if err := store.EnqueueJob(job); err != nil {
		t.Fatalf("falha ao enfileirar job: %v", err)
	}

	res, err := runner.ExecuteJob(context.Background(), job)
	if err != nil {
		t.Fatalf("esperava sucesso na execução do job, obteve erro: %v", err)
	}

	if !res.Success {
		t.Errorf("esperava res.Success == true, obteve false")
	}
	if step1.Count() != 1 {
		t.Errorf("esperava step1 executado 1 vez, foi %d", step1.Count())
	}
	if step2.Count() != 1 {
		t.Errorf("esperava step2 executado 1 vez, foi %d", step2.Count())
	}

	// Verifica se o watcher foi atualizado no banco
	w, err := store.GetWatcherByName("vault-notes")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != queue.WatcherHealthy {
		t.Errorf("status esperado HEALTHY, obteve %s", w.Status)
	}
	if w.LastSuccessSyncAt == nil {
		t.Errorf("esperava LastSuccessSyncAt preenchido")
	}
}

func TestPipelineRunner_AbortSubsequentStepsOnFailure(t *testing.T) {
	store := setupTestStore(t)
	reg := providers.NewRegistry()

	step1 := &mockAction{name: "step.one"}
	step2 := &mockAction{
		name: "step.two",
		executeFunc: func(ctx *providers.StepContext) (*providers.StepResult, error) {
			return &providers.StepResult{
				Success:      false,
				ErrorMessage: "falha simulada no step 2",
			}, errors.New("erro interno no step 2")
		},
	}
	step3 := &mockAction{name: "step.three"}
	step4 := &mockAction{name: "step.four"}

	_ = reg.Register(step1)
	_ = reg.Register(step2)
	_ = reg.Register(step3)
	_ = reg.Register(step4)

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "vault-notes", Path: "/tmp/test-vault"},
		},
		Pipelines: map[string]config.Pipeline{
			"chain_pipe": {
				Timeout: "5s",
				Steps: []config.Step{
					{Action: "step.one"},
					{Action: "step.two"},
					{Action: "step.three"},
					{Action: "step.four"},
				},
			},
		},
	}

	runner := pipeline.NewRunner(store, reg, cfg)

	job := &queue.Job{
		ID:           "job-abort-test",
		WatcherID:    "vault-notes",
		PipelineName: "chain_pipe",
		PayloadFiles: []string{"file.txt"},
		MaxRetries:   3,
	}
	_ = store.EnqueueJob(job)

	res, err := runner.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("esperava erro na execução do pipeline, obteve sucesso")
	}

	if res.Success {
		t.Errorf("esperava res.Success == false")
	}
	if res.ErrorStep != "step.two" {
		t.Errorf("esperava ErrorStep 'step.two', obteve '%s'", res.ErrorStep)
	}

	// Validação rigorosa dos passos executados
	if step1.Count() != 1 {
		t.Errorf("step1 deveria ter executado 1 vez, foi %d", step1.Count())
	}
	if step2.Count() != 1 {
		t.Errorf("step2 deveria ter executado 1 vez, foi %d", step2.Count())
	}
	if step3.Count() != 0 {
		t.Errorf("CRITÉRIO DE ACEITE VIOLADO: step3 foi executado %d vezes (deveria ser 0)", step3.Count())
	}
	if step4.Count() != 0 {
		t.Errorf("CRITÉRIO DE ACEITE VIOLADO: step4 foi executado %d vezes (deveria ser 0)", step4.Count())
	}
}

func TestPipelineRunner_MergeConflictClassification(t *testing.T) {
	store := setupTestStore(t)
	reg := providers.NewRegistry()

	conflictStep := &mockAction{
		name: "git.safe_sync",
		executeFunc: func(ctx *providers.StepContext) (*providers.StepResult, error) {
			return &providers.StepResult{
				Success:      false,
				ConflictErr:  true,
				ErrorMessage: "conflito de merge detectado entre origin e local",
			}, providers.ErrConflict
		},
	}
	_ = reg.Register(conflictStep)

	runner := pipeline.NewRunner(store, reg, nil)
	runner.RegisterWatcher(config.WatcherConfig{Name: "vault-notes", Path: "/tmp/test-vault"})
	runner.RegisterPipeline("conflict_pipe", config.Pipeline{
		Steps: []config.Step{{Action: "git.safe_sync"}},
	})

	job := &queue.Job{
		ID:           "job-conflict",
		WatcherID:    "vault-notes",
		PipelineName: "conflict_pipe",
		PayloadFiles: []string{"clash.md"},
	}
	_ = store.EnqueueJob(job)

	res, err := runner.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("esperava erro de conflito, obteve nil")
	}

	if res.Category != pipeline.CategoryConflict {
		t.Errorf("esperava CategoryConflict, obteve %v", res.Category)
	}

	// Verifica se o watcher foi colocado em CONFLICT_HALTED
	w, err := store.GetWatcherByName("vault-notes")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != queue.WatcherConflictHalted {
		t.Errorf("esperava status CONFLICT_HALTED no watcher, obteve %s", w.Status)
	}
}

func TestPipelineRunner_TransientErrorAndRetry(t *testing.T) {
	store := setupTestStore(t)
	reg := providers.NewRegistry()

	transientStep := &mockAction{
		name: "git.push",
		executeFunc: func(ctx *providers.StepContext) (*providers.StepResult, error) {
			return &providers.StepResult{
				Success:      false,
				TransientErr: true,
				ErrorMessage: "connection refused: remote unreachable",
			}, errors.New("network is unreachable")
		},
	}
	_ = reg.Register(transientStep)

	runner := pipeline.NewRunner(store, reg, nil)
	runner.RegisterWatcher(config.WatcherConfig{Name: "vault-notes", Path: "/tmp/test-vault"})
	runner.RegisterPipeline("transient_pipe", config.Pipeline{
		Steps: []config.Step{{Action: "git.push"}},
	})

	job := &queue.Job{
		ID:           "job-transient",
		WatcherID:    "vault-notes",
		PipelineName: "transient_pipe",
		PayloadFiles: []string{"offline.md"},
		RetryCount:   0,
		MaxRetries:   3,
	}
	_ = store.EnqueueJob(job)

	res, err := runner.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("esperava erro transitório, obteve nil")
	}

	if res.Category != pipeline.CategoryTransient {
		t.Errorf("esperava CategoryTransient, obteve %v", res.Category)
	}

	// Watcher deve ter ido para DEGRADED
	w, err := store.GetWatcherByName("vault-notes")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != queue.WatcherDegraded {
		t.Errorf("esperava status DEGRADED, obteve %s", w.Status)
	}
}

func TestPipelineRunner_TimeoutAbortsPipeline(t *testing.T) {
	store := setupTestStore(t)
	reg := providers.NewRegistry()

	slowStep := &mockAction{
		name: "slow.action",
		executeFunc: func(ctx *providers.StepContext) (*providers.StepResult, error) {
			select {
			case <-ctx.Context.Done():
				return nil, ctx.Context.Err()
			case <-time.After(500 * time.Millisecond):
				return &providers.StepResult{Success: true}, nil
			}
		},
	}
	_ = reg.Register(slowStep)

	runner := pipeline.NewRunner(store, reg, nil)
	runner.RegisterWatcher(config.WatcherConfig{Name: "vault-notes", Path: "/tmp/test-vault"})
	runner.RegisterPipeline("timeout_pipe", config.Pipeline{
		Timeout: "50ms",
		Steps:   []config.Step{{Action: "slow.action"}},
	})

	job := &queue.Job{
		ID:           "job-timeout",
		WatcherID:    "vault-notes",
		PipelineName: "timeout_pipe",
		PayloadFiles: []string{"file.txt"},
		MaxRetries:   3,
	}
	_ = store.EnqueueJob(job)

	res, err := runner.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("esperava erro de timeout, obteve nil")
	}

	if res.Category != pipeline.CategoryTransient {
		t.Errorf("esperava erro de timeout classificado como transitório, obteve %v", res.Category)
	}
}

func TestPipelineRunner_MissingActionOrPipeline(t *testing.T) {
	store := setupTestStore(t)
	runner := pipeline.NewRunner(store, nil, nil)
	runner.RegisterWatcher(config.WatcherConfig{Name: "vault-notes", Path: "/tmp/test-vault"})

	// Pipeline não configurado
	jobMissingPipe := &queue.Job{
		ID:           "job-missing-pipe",
		WatcherID:    "vault-notes",
		PipelineName: "non_existent_pipeline",
	}
	_, err := runner.ExecuteJob(context.Background(), jobMissingPipe)
	if err == nil {
		t.Fatalf("esperava erro para pipeline inexistente")
	}

	// Action não cadastrada no catálogo
	runner.RegisterPipeline("orphan_step_pipe", config.Pipeline{
		Steps: []config.Step{{Action: "ghost.action"}},
	})
	jobMissingAction := &queue.Job{
		ID:           "job-missing-action",
		WatcherID:    "vault-notes",
		PipelineName: "orphan_step_pipe",
	}
	_, err = runner.ExecuteJob(context.Background(), jobMissingAction)
	if err == nil {
		t.Fatalf("esperava erro para ação não cadastrada")
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		res      *providers.StepResult
		expected pipeline.ErrorCategory
	}{
		{
			name:     "Sucesso",
			err:      nil,
			res:      &providers.StepResult{Success: true},
			expected: pipeline.CategoryTransient,
		},
		{
			name:     "Conflito por erro sentinela",
			err:      providers.ErrConflict,
			res:      nil,
			expected: pipeline.CategoryConflict,
		},
		{
			name:     "Conflito por flag no StepResult",
			err:      errors.New("algum erro"),
			res:      &providers.StepResult{ConflictErr: true},
			expected: pipeline.CategoryConflict,
		},
		{
			name:     "Conflito por texto",
			err:      errors.New("automatic merge failed; fix conflicts and then commit"),
			res:      nil,
			expected: pipeline.CategoryConflict,
		},
		{
			name:     "Transitório por index.lock",
			err:      errors.New("fatal: Unable to create '.git/index.lock': File exists"),
			res:      nil,
			expected: pipeline.CategoryTransient,
		},
		{
			// Regressão: duas máquinas empurrando para o mesmo ref fazem o
			// servidor recusar o lock da perdedora. A mensagem do GitHub é
			// "[remote rejected]", que NÃO contém a substring "[rejected]" da
			// lista de transitórios — sem indicador próprio isso caía em FATAL
			// e o job morria sem nenhuma das 5 tentativas.
			name: "Transitório por disputa do ref remoto",
			err: errors.New(`git push origin main: To https://github.com/exemplo/cofre.git
 ! [remote rejected] main -> main (cannot lock ref 'refs/heads/main': is at 8666370318d6a5ac6afaeab8fe47cd339baa1163 but expected dca3986370808a1ad0d8a01012b4e16b46873064)
error: failed to push some refs to 'https://github.com/exemplo/cofre.git'`),
			res:      nil,
			expected: pipeline.CategoryTransient,
		},
		{
			name:     "Transitório por timeout",
			err:      context.DeadlineExceeded,
			res:      nil,
			expected: pipeline.CategoryTransient,
		},
		{
			name:     "Fatal por repositório inexistente",
			err:      errors.New("fatal: not a git repository"),
			res:      nil,
			expected: pipeline.CategoryFatal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pipeline.ClassifyError(tc.err, tc.res)
			if got != tc.expected {
				t.Errorf("esperava %v, obteve %v", tc.expected, got)
			}
		})
	}
}

func TestCalculateBackoff(t *testing.T) {
	for retry := 0; retry <= 6; retry++ {
		backoff := pipeline.CalculateBackoff(retry)
		minExpected := time.Duration(5*(1<<retry)) * time.Second
		maxExpected := minExpected + 3*time.Second

		if retry >= 6 {
			// No limite superior pode atingir o teto de 300s
			if backoff > 300*time.Second {
				t.Errorf("para retry=%d, backoff excedeu o teto máximo de 300s: %v", retry, backoff)
			}
		} else {
			if backoff < minExpected || backoff > maxExpected {
				t.Errorf("para retry=%d, backoff fora do intervalo esperado [%v, %v]: %v", retry, minExpected, maxExpected, backoff)
			}
		}
	}
}
