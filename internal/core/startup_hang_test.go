package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/internal/watcher"
)

// blockingFactory simula o registro de watcher que trava (issue #4): entra,
// sinaliza e só prossegue quando release fecha.
func blockingFactory(t *testing.T) (entered, release chan struct{}) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	restore := core.SetWatcherFactory(func(name, root string) (*watcher.Watcher, error) {
		close(entered)
		<-release
		return watcher.New(name, root)
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		restore()
	})
	return entered, release
}

func waitClosed(t *testing.T, ch <-chan struct{}, limit time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(limit):
		t.Fatalf("tempo esgotado aguardando %s", what)
	}
}

func shutdown(t *testing.T, coord *core.Coordinator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := coord.Shutdown(ctx); err != nil {
		t.Fatalf("falha no shutdown: %v", err)
	}
}

func watcherStatus(t *testing.T, coord *core.Coordinator, name string) (*string, bool) {
	t.Helper()
	res, err := coord.Status(context.Background())
	if err != nil {
		t.Fatalf("falha ao consultar status: %v", err)
	}
	for _, w := range res.Watchers {
		if w.Name == name {
			s := w.Status
			return &s, res.Starting
		}
	}
	return nil, res.Starting
}

// Durante o registro do watcher o IPC já responde; o status não pode dizer
// HEALTHY, porque nenhuma alteração está sendo capturada.
func TestStatusReportsStartingUntilWatchersRegistered(t *testing.T) {
	entered, release := blockingFactory(t)
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = coord.Start(context.Background()) }()
	defer shutdown(t, coord)

	waitClosed(t, entered, 5*time.Second, "o registro do watcher começar")

	status, starting := watcherStatus(t, coord, "vault")
	if !starting {
		t.Error("daemon reportado como pronto com o registro do watcher em andamento")
	}
	if status == nil || *status != string(queue.WatcherStarting) {
		t.Errorf("status do watcher durante o boot = %v; esperado %s", status, queue.WatcherStarting)
	}

	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, starting = watcherStatus(t, coord, "vault"); !starting {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if starting {
		t.Fatal("daemon não saiu de INICIANDO depois de o watcher registrar")
	}
	if status == nil || *status != string(queue.WatcherHealthy) {
		t.Errorf("status do watcher após o boot = %v; esperado %s", status, queue.WatcherHealthy)
	}
}

// Um registro que não termina precisa virar falha do Start, para que o
// supervisor reinicie o daemon em vez de ele seguir de pé sem vigiar nada. Se
// o registro abandonado concluir depois, o watcher não pode ser ativado.
func TestStartFailsWhenWatcherRegistrationTimesOut(t *testing.T) {
	defer core.SetWatcherStartTimeout(200 * time.Millisecond)()
	_, release := blockingFactory(t)
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- coord.Start(context.Background()) }()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Start retornou sem erro apesar do registro travado")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start continuou bloqueado após o prazo de registro")
	}

	close(release)
	time.Sleep(300 * time.Millisecond)
	if coord.IsCapturing("vault") {
		t.Error("watcher abandonado pelo boot foi ativado ao concluir tardiamente")
	}

	shutdown(t, coord)
}

// 'watchflow stop' com o boot travado: antes, só o contexto era cancelado e
// ninguém que esperava o Start acordava.
func TestStopUnblocksStartDuringHungRegistration(t *testing.T) {
	entered, _ := blockingFactory(t)
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- coord.Start(context.Background()) }()
	waitClosed(t, entered, 5*time.Second, "o registro do watcher começar")

	if _, err := coord.Stop(context.Background()); err != nil {
		t.Fatalf("stop recusado: %v", err)
	}
	waitClosed(t, coord.StopRequested(), 2*time.Second, "o sinal de parada")

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("parada pedida durante o boot tratada como falha: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start não retornou após o stop")
	}

	shutdown(t, coord)
}

// Um worker que ignora o cancelamento não pode pendurar o encerramento.
func TestShutdownDoesNotWaitForeverOnStuckWorker(t *testing.T) {
	defer core.SetWorkerExitGrace(200 * time.Millisecond)()

	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	action := &stuckAction{name: "test.stuck.shutdown", started: make(chan struct{}), unblock: unblock}
	if err := providers.DefaultRegistry.Register(action); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { providers.DefaultRegistry.Unregister(action.name) })

	cfg, _ := newTestConfig(t, "stuck-pipe", []config.Step{{Action: action.name}})
	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = coord.Start(context.Background()) }()
	time.Sleep(200 * time.Millisecond)

	if _, err := coord.Sync(context.Background(), "vault"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, action.started, 5*time.Second, "o worker iniciar o step")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = coord.Shutdown(ctx)
		close(done)
	}()
	waitClosed(t, done, 5*time.Second, "o shutdown concluir com um worker preso")
}

// stuckAction ignora o contexto, como um worker preso fora do git.
type stuckAction struct {
	name    string
	started chan struct{}
	unblock chan struct{}
}

func (a *stuckAction) Name() string                          { return a.name }
func (a *stuckAction) Validate(map[string]interface{}) error { return nil }

func (a *stuckAction) Execute(*providers.StepContext) (*providers.StepResult, error) {
	close(a.started)
	<-a.unblock
	return &providers.StepResult{Success: true}, nil
}
