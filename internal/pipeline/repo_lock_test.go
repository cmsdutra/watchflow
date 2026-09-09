package pipeline_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
)

// tracingAction registra a sequência global de entrada e saída dos steps para
// permitir detectar intercalação entre pipelines concorrentes.
type tracingAction struct {
	name  string
	delay time.Duration
	trace *executionTrace
}

type executionTrace struct {
	mu     sync.Mutex
	events []string
}

func (tr *executionTrace) record(format string, args ...interface{}) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.events = append(tr.events, fmt.Sprintf(format, args...))
}

func (tr *executionTrace) snapshot() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]string, len(tr.events))
	copy(out, tr.events)
	return out
}

func (a *tracingAction) Name() string { return a.name }

func (a *tracingAction) Validate(map[string]interface{}) error { return nil }

func (a *tracingAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	a.trace.record("enter:%s:%s", ctx.WatcherName, a.name)
	time.Sleep(a.delay)
	a.trace.record("exit:%s:%s", ctx.WatcherName, a.name)

	return &providers.StepResult{
		Success: true,
		Output:  fmt.Sprintf("%s ok", a.name),
	}, nil
}

// TestConcurrentPipelinesOnSameRepoDoNotInterleave cobre a regressão do
// a invariante de travas (README). Com a trava adquirida por action (e liberada entre steps),
// dois pipelines sobre a mesma árvore Git intercalavam seus steps — cenário em
// que o commit de um pipeline arrasta o stage do outro.
func TestConcurrentPipelinesOnSameRepoDoNotInterleave(t *testing.T) {
	sharedRepo := t.TempDir()

	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao inicializar SQLite em memória: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	watcherNames := []string{"vault-a", "vault-b"}
	for _, name := range watcherNames {
		if err := store.RegisterWatcher(&queue.WatcherRecord{
			ID: name, Name: name, Path: sharedRepo, Status: queue.WatcherHealthy,
		}); err != nil {
			t.Fatalf("falha ao registrar watcher '%s': %v", name, err)
		}
	}

	trace := &executionTrace{}
	reg := providers.NewRegistry()
	for _, step := range []string{"fake.add", "fake.commit", "fake.push"} {
		if err := reg.Register(&tracingAction{name: step, delay: 30 * time.Millisecond, trace: trace}); err != nil {
			t.Fatal(err)
		}
	}

	// Dois watchers distintos apontando para o MESMO repositório — configuração
	// válida e o caminho mais direto para a corrida.
	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "vault-a", Path: sharedRepo, ResolvedPath: sharedRepo},
			{Name: "vault-b", Path: sharedRepo, ResolvedPath: sharedRepo},
		},
		Pipelines: map[string]config.Pipeline{
			"git_sync": {
				Timeout:         "30s",
				TimeoutDuration: 30 * time.Second,
				Steps: []config.Step{
					{Action: "fake.add"},
					{Action: "fake.commit"},
					{Action: "fake.push"},
				},
			},
		},
	}

	runner := pipeline.NewRunner(store, reg, cfg)

	var wg sync.WaitGroup
	for i, name := range watcherNames {
		job := &queue.Job{
			ID:           fmt.Sprintf("job-lock-%d", i),
			WatcherID:    name,
			PipelineName: "git_sync",
			PayloadFiles: []string{"nota.md"},
			MaxRetries:   3,
		}
		if err := store.EnqueueJob(job); err != nil {
			t.Fatalf("falha ao enfileirar job: %v", err)
		}

		wg.Add(1)
		go func(j *queue.Job) {
			defer wg.Done()
			if _, err := runner.ExecuteJob(context.Background(), j); err != nil {
				t.Errorf("pipeline de '%s' falhou: %v", j.WatcherID, err)
			}
		}(job)
	}
	wg.Wait()

	events := trace.snapshot()
	if len(events) != 12 {
		t.Fatalf("esperava 12 eventos de trace (2 pipelines x 3 steps x 2), obteve %d: %v", len(events), events)
	}

	// O primeiro watcher a entrar deve concluir os 3 steps antes de o outro
	// executar qualquer step.
	firstWatcher := splitWatcher(events[0])
	if firstWatcher == "" {
		t.Fatalf("trace inesperado: %q", events[0])
	}

	for i := 0; i < 6; i++ {
		if got := splitWatcher(events[i]); got != firstWatcher {
			t.Fatalf("intercalação detectada: evento %d (%q) é do watcher '%s', esperado '%s'.\nTrace completo: %v",
				i, events[i], got, firstWatcher, events)
		}
	}
}

// splitWatcher extrai o nome do watcher de um evento "verbo:watcher:action".
func splitWatcher(event string) string {
	var verb, watcher, action string
	parts := 0
	start := 0
	fields := []*string{&verb, &watcher, &action}
	for i := 0; i <= len(event); i++ {
		if i == len(event) || event[i] == ':' {
			if parts < len(fields) {
				*fields[parts] = event[start:i]
			}
			parts++
			start = i + 1
		}
	}
	return watcher
}

// TestRepoLockHeldIsPropagatedToSteps garante que o Runner sinaliza às actions
// que a trava já está retida, evitando auto-deadlock em sync.Mutex.
func TestRepoLockHeldIsPropagatedToSteps(t *testing.T) {
	repo := t.TempDir()

	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.RegisterWatcher(&queue.WatcherRecord{
		ID: "vault", Name: "vault", Path: repo, Status: queue.WatcherHealthy,
	}); err != nil {
		t.Fatal(err)
	}

	var seenHeld bool
	reg := providers.NewRegistry()
	if err := reg.Register(&inspectAction{
		name: "inspect.lock",
		fn: func(ctx *providers.StepContext) {
			seenHeld = ctx.RepoLockHeld
		},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{{Name: "vault", Path: repo, ResolvedPath: repo}},
		Pipelines: map[string]config.Pipeline{
			"p": {Timeout: "5s", TimeoutDuration: 5 * time.Second, Steps: []config.Step{{Action: "inspect.lock"}}},
		},
	}

	job := &queue.Job{ID: "job-held", WatcherID: "vault", PipelineName: "p", MaxRetries: 3}
	if err := store.EnqueueJob(job); err != nil {
		t.Fatal(err)
	}

	if _, err := pipeline.NewRunner(store, reg, cfg).ExecuteJob(context.Background(), job); err != nil {
		t.Fatalf("execução falhou: %v", err)
	}

	if !seenHeld {
		t.Error("esperava RepoLockHeld == true no StepContext; sem isso as actions readquirem a trava do pipeline e causam deadlock")
	}
}

type inspectAction struct {
	name string
	fn   func(*providers.StepContext)
}

func (a *inspectAction) Name() string                          { return a.name }
func (a *inspectAction) Validate(map[string]interface{}) error { return nil }
func (a *inspectAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	a.fn(ctx)
	return &providers.StepResult{Success: true}, nil
}
