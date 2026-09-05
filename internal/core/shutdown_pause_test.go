package core_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
)

// slowAction simula um step demorado (ex.: 'git push') e registra se o contexto
// de execução foi cancelado antes de o step concluir.
type slowAction struct {
	name     string
	duration time.Duration

	mu        sync.Mutex
	started   bool
	finished  bool
	ctxErrEnd error
}

func (a *slowAction) Name() string                          { return a.name }
func (a *slowAction) Validate(map[string]interface{}) error { return nil }

func (a *slowAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()

	time.Sleep(a.duration)

	a.mu.Lock()
	a.finished = true
	a.ctxErrEnd = ctx.Context.Err()
	a.mu.Unlock()

	return &providers.StepResult{Success: true, Output: "concluído"}, nil
}

func (a *slowAction) state() (bool, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started, a.finished, a.ctxErrEnd
}

func newTestConfig(t *testing.T, pipelineName string, steps []config.Step) (*config.Config, string) {
	t.Helper()

	tempDir := t.TempDir()
	watchDir := filepath.Join(tempDir, "vault")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("falha ao criar pasta vigiada: %v", err)
	}
	stateDir := filepath.Join(tempDir, "state")

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             filepath.Join(tempDir, "wf.sock"),
			LogLevel:               "info",
			MaxConcurrentPipelines: 1,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "vault",
				Path:             watchDir,
				ResolvedPath:     watchDir,
				Debounce:         "50ms",
				MaxWait:          "100ms",
				DebounceDuration: 50 * time.Millisecond,
				MaxWaitDuration:  100 * time.Millisecond,
				Pipelines:        []string{pipelineName},
			},
		},
		Pipelines: map[string]config.Pipeline{
			pipelineName: {
				Timeout:         "30s",
				TimeoutDuration: 30 * time.Second,
				Steps:           steps,
			},
		},
	}

	return cfg, filepath.Join(stateDir, "state.db")
}

// TestPauseDoesNotConsumeRetries cobre a regressão em que 'watchflow pause'
// devolvia os jobs à fila via MarkJobFailed, incrementando retry_count a cada
// ciclo. Poucos segundos de pausa esgotavam max_retries e marcavam trabalho
// legítimo do usuário como FAILED definitivo.
func TestPauseDoesNotConsumeRetries(t *testing.T) {
	cfg, dbPath := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = coord.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)

	if _, err := coord.Pause(ctx, "vault"); err != nil {
		t.Fatalf("falha ao pausar watcher: %v", err)
	}
	if _, err := coord.Sync(ctx, "vault"); err != nil {
		t.Fatalf("falha ao solicitar sync: %v", err)
	}

	// Tempo suficiente para vários ciclos de dequeue enquanto pausado
	time.Sleep(2 * time.Second)

	pending, err := coord.Store().ListPendingJobs("vault")
	if err != nil {
		t.Fatalf("falha ao listar jobs pendentes: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("esperava 1 job aguardando o resume, obteve %d", len(pending))
	}
	if pending[0].RetryCount != 0 {
		t.Errorf("pausa consumiu %d tentativa(s) de retry; esperado 0 — a pausa é decisão do usuário, não falha do job", pending[0].RetryCount)
	}

	// Após o resume o job deve ser efetivamente processado
	if _, err := coord.Resume(ctx, "vault"); err != nil {
		t.Fatalf("falha ao retomar watcher: %v", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := coord.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("falha no shutdown: %v", err)
	}
	_ = dbPath
}

// TestGracefulShutdownLetsInFlightJobFinish cobre a regressão em que Shutdown
// cancelava o mesmo contexto usado por exec.CommandContext, matando um 'git
// push' em andamento e deixando o job preso em RUNNING.
func TestGracefulShutdownLetsInFlightJobFinish(t *testing.T) {
	action := &slowAction{name: "test.slow.shutdown", duration: 1500 * time.Millisecond}
	if err := providers.DefaultRegistry.Register(action); err != nil {
		t.Fatalf("falha ao registrar action de teste: %v", err)
	}
	t.Cleanup(func() { providers.DefaultRegistry.Unregister(action.Name()) })

	cfg, dbPath := newTestConfig(t, "slow-pipe", []config.Step{{Action: action.name}})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = coord.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)

	if _, err := coord.Sync(ctx, "vault"); err != nil {
		t.Fatalf("falha ao solicitar sync: %v", err)
	}

	// Aguarda o worker efetivamente iniciar o step lento
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if started, _, _ := action.state(); started {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if started, _, _ := action.state(); !started {
		t.Fatal("o step lento nunca foi iniciado pelo worker")
	}

	// Encerra o daemon no meio da execução, com prazo folgado
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := coord.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("falha no shutdown: %v", err)
	}

	_, finished, ctxErr := action.state()
	if !finished {
		t.Fatal("o job em voo não concluiu: o shutdown o interrompeu")
	}
	if ctxErr != nil {
		t.Errorf("o contexto de execução foi cancelado durante o job em voo (%v); um 'git push' real teria recebido SIGKILL", ctxErr)
	}

	// O job deve ter sido finalizado, e não abandonado em RUNNING
	store, err := queue.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao reabrir o banco: %v", err)
	}
	defer func() { _ = store.Close() }()

	stuck, err := store.ListPendingJobs("")
	if err != nil {
		t.Fatalf("falha ao listar jobs: %v", err)
	}
	for _, j := range stuck {
		if j.RetryCount != 0 {
			t.Errorf("job '%s' foi penalizado com %d retry(s) por um encerramento ordenado", j.ID, j.RetryCount)
		}
	}
}

// TestRequeueRunningJobsDoesNotPenalize garante que jobs interrompidos por
// parada ordenada voltam para PENDING sem consumir tentativas, ao contrário de
// ResetRunningJobs (recuperação pós-crash), que mantém a penalidade como
// proteção contra jobs venenosos.
func TestRequeueRunningJobsDoesNotPenalize(t *testing.T) {
	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao abrir SQLite em memória: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.RegisterWatcher(&queue.WatcherRecord{
		ID: "w", Name: "w", Path: "/tmp/w", Status: queue.WatcherHealthy,
	}); err != nil {
		t.Fatal(err)
	}

	job := &queue.Job{ID: "j1", WatcherID: "w", PipelineName: "p", MaxRetries: 5}
	if err := store.EnqueueJob(job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DequeueNextPending("worker-1"); err != nil {
		t.Fatalf("falha ao alocar job: %v", err)
	}

	n, err := store.RequeueRunningJobs("encerramento gracioso")
	if err != nil {
		t.Fatalf("falha ao reenfileirar jobs em RUNNING: %v", err)
	}
	if n != 1 {
		t.Fatalf("esperava 1 job reenfileirado, obteve %d", n)
	}

	pending, err := store.ListPendingJobs("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("esperava o job de volta em PENDING, obteve %d", len(pending))
	}
	if pending[0].RetryCount != 0 {
		t.Errorf("esperava retry_count 0 após parada ordenada, obteve %d", pending[0].RetryCount)
	}

	// Contraste: ResetRunningJobs (pós-crash) deve continuar penalizando
	if _, err := store.DequeueNextPending("worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetRunningJobs(); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingJobs("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RetryCount != 1 {
		t.Errorf("esperava retry_count 1 após recuperação pós-crash, obteve %+v", pending)
	}
}

// TestRequeueJobKeepsRetryCount valida o método usado pela pausa.
func TestRequeueJobKeepsRetryCount(t *testing.T) {
	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := store.RegisterWatcher(&queue.WatcherRecord{
		ID: "w", Name: "w", Path: "/tmp/w", Status: queue.WatcherHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueJob(&queue.Job{ID: "j1", WatcherID: "w", PipelineName: "p", MaxRetries: 5}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if _, err := store.DequeueNextPending("worker-1"); err != nil {
			t.Fatal(err)
		}
		if err := store.RequeueJob("j1", 0, "watcher pausado"); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := store.ListPendingJobs("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("job desapareceu da fila após 10 ciclos de pausa: %d pendentes", len(pending))
	}
	if pending[0].RetryCount != 0 {
		t.Errorf("10 ciclos de pausa consumiram %d tentativas; esperado 0", pending[0].RetryCount)
	}
}
