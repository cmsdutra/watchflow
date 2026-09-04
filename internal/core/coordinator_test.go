package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/tests/testutil"
)

func TestCoordinator_LifecycleAndRPC(t *testing.T) {
	tempDir := t.TempDir()
	watchDir := filepath.Join(tempDir, "watched_vault")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("falha ao criar pasta vigiada: %v", err)
	}

	sockPath := filepath.Join(tempDir, "watchflow.sock")
	stateDir := filepath.Join(tempDir, "state")

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             sockPath,
			LogLevel:               "info",
			MaxConcurrentPipelines: 2,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "test-vault",
				Path:             watchDir,
				ResolvedPath:     watchDir,
				Debounce:         "50ms",
				MaxWait:          "100ms",
				DebounceDuration: 50 * time.Millisecond,
				MaxWaitDuration:  100 * time.Millisecond,
				Pipelines:        []string{"noop-pipeline"},
			},
		},
		Pipelines: map[string]config.Pipeline{
			"noop-pipeline": {
				Timeout:         "10s",
				TimeoutDuration: 10 * time.Second,
				Steps:           []config.Step{},
			},
		},
	}

	coord, err := core.NewCoordinator(cfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- coord.Start(ctx)
	}()

	// Aguarda startup do socket Unix
	time.Sleep(150 * time.Millisecond)

	// 1. Testa Status via chamada direta do Coordinator
	statusResp, err := coord.Status(ctx)
	if err != nil {
		t.Fatalf("erro ao consultar status: %v", err)
	}
	if statusResp.DaemonPID != os.Getpid() {
		t.Errorf("PID divergente: esperado %d, obtido %d", os.Getpid(), statusResp.DaemonPID)
	}
	if len(statusResp.Watchers) != 1 || statusResp.Watchers[0].Name != "test-vault" {
		t.Fatalf("watchers inesperados no status: %+v", statusResp.Watchers)
	}
	if statusResp.Watchers[0].Status != string(queue.WatcherHealthy) {
		t.Errorf("status do watcher inesperado: %s", statusResp.Watchers[0].Status)
	}

	// 2. Testa Status via Client IPC conectado ao Unix socket
	client := ipc.NewClient(sockPath)
	clientStatus, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("falha na consulta RPC de status: %v", err)
	}
	if clientStatus.Version != "0.1.0-test" {
		t.Errorf("versão RPC inesperada: %s", clientStatus.Version)
	}

	// 3. Testa Pause via IPC
	pauseResp, err := client.Pause(ctx, "test-vault")
	if err != nil || !pauseResp.Success {
		t.Fatalf("falha ao pausar watcher via IPC: %v", err)
	}
	statusAfterPause, _ := client.Status(ctx)
	if statusAfterPause.Watchers[0].Status != string(queue.WatcherPaused) {
		t.Errorf("status esperado após pausa: PAUSED, obtido: %s", statusAfterPause.Watchers[0].Status)
	}

	// 4. Testa Resume via IPC
	resumeResp, err := client.Resume(ctx, "test-vault")
	if err != nil || !resumeResp.Success {
		t.Fatalf("falha ao retomar watcher via IPC: %v", err)
	}
	statusAfterResume, _ := client.Status(ctx)
	if statusAfterResume.Watchers[0].Status != string(queue.WatcherHealthy) {
		t.Errorf("status esperado após retomada: HEALTHY, obtido: %s", statusAfterResume.Watchers[0].Status)
	}

	// 5. Testa Sync manual via IPC
	syncResp, err := client.Sync(ctx, "test-vault")
	if err != nil {
		t.Fatalf("falha ao solicitar sync via IPC: %v", err)
	}
	if len(syncResp.EnqueuedJobs) == 0 {
		t.Errorf("esperava jobs enfileirados no sync manual")
	}

	// 6. Testa Stop via IPC
	stopResp, err := client.Stop(ctx)
	if err != nil || !stopResp.Success {
		t.Fatalf("falha ao solicitar stop via IPC: %v", err)
	}

	// 7. Encerramento gracioso
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()

	if err := coord.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("falha no shutdown gracioso: %v", err)
	}
}

func TestCoordinator_ProcessLock(t *testing.T) {
	tempDir := t.TempDir()
	watchDir := filepath.Join(tempDir, "watched_vault")
	_ = os.MkdirAll(watchDir, 0755)

	sockPath := filepath.Join(tempDir, "watchflow.sock")
	stateDir := filepath.Join(tempDir, "state")

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             sockPath,
			LogLevel:               "info",
			MaxConcurrentPipelines: 1,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "test-vault",
				Path:             watchDir,
				ResolvedPath:     watchDir,
				Debounce:         "50ms",
				MaxWait:          "100ms",
				DebounceDuration: 50 * time.Millisecond,
				MaxWaitDuration:  100 * time.Millisecond,
			},
		},
		Pipelines: map[string]config.Pipeline{},
	}

	coord1, err := core.NewCoordinator(cfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador 1: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	go func() {
		_ = coord1.Start(ctx1)
	}()

	time.Sleep(150 * time.Millisecond)

	// Instância 2 tentando usar o mesmo socket
	stateDir2 := filepath.Join(tempDir, "state2")
	cfg2 := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir2,
			SocketPath:             sockPath,
			LogLevel:               "info",
			MaxConcurrentPipelines: 1,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "test-vault-2",
				Path:             watchDir,
				ResolvedPath:     watchDir,
				Debounce:         "50ms",
				MaxWait:          "100ms",
				DebounceDuration: 50 * time.Millisecond,
				MaxWaitDuration:  100 * time.Millisecond,
			},
		},
		Pipelines: map[string]config.Pipeline{},
	}

	coord2, err := core.NewCoordinator(cfg2, "0.1.0-test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador 2: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	err2 := coord2.Start(ctx2)
	if err2 == nil {
		t.Fatalf("esperava erro de inicialização para coord2 disputando socket, mas iniciou com sucesso")
	}
	if !strings.Contains(err2.Error(), "outra instância") && !strings.Contains(err2.Error(), "servidor IPC") {
		t.Errorf("mensagem de erro inesperada: %v", err2)
	}

	// Encerra instância 1
	_ = coord1.Shutdown(context.Background())
}

func TestCoordinator_ReactivePipelineExecution(t *testing.T) {
	sandbox := testutil.NewGitSandbox(t)
	sandbox.WriteFile("README.md", "# Sandbox Vault")
	sandbox.CommitAll("initial commit")

	tempDir := t.TempDir()

	sockPath := filepath.Join(tempDir, "watchflow.sock")
	stateDir := filepath.Join(tempDir, "state")

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             sockPath,
			LogLevel:               "info",
			MaxConcurrentPipelines: 1,
		},
		Watchers: []config.WatcherConfig{
			{
				Name:             "sandbox-vault",
				Path:             sandbox.RootDir,
				ResolvedPath:     sandbox.RootDir,
				Debounce:         "50ms",
				MaxWait:          "100ms",
				DebounceDuration: 50 * time.Millisecond,
				MaxWaitDuration:  100 * time.Millisecond,
				Pipelines:        []string{"git-local-sync"},
			},
		},
		Pipelines: map[string]config.Pipeline{
			"git-local-sync": {
				Timeout:         "10s",
				TimeoutDuration: 10 * time.Second,
				Steps: []config.Step{
					{Action: "git.check_locks"},
					{Action: "git.add"},
					{
						Action: "git.commit",
						Params: map[string]interface{}{
							"message": "watchflow: auto-commit reactive",
						},
					},
				},
			},
		},
	}

	coord, err := core.NewCoordinator(cfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = coord.Start(ctx)
	}()

	time.Sleep(150 * time.Millisecond)

	// Cria arquivo no repositório monitorado
	sandbox.WriteFile("ideia.md", "# Minha Ideia Reativa\nConteúdo salvo.")

	// Aguarda processamento do debouncer (50ms) + enfileiramento e execução do worker (300ms)
	time.Sleep(1200 * time.Millisecond)

	// Inspeciona estado do banco para depuração se necessário
	watchers, _ := coord.Store().ListWatchers()
	for _, w := range watchers {
		t.Logf("Watcher: %s, Status: %s, LastErr: %s, LastEvent: %v", w.Name, w.Status, w.LastErrorMessage, w.LastEventAt)
	}

	// Verifica se o commit foi gerado automaticamente pelo pipeline
	logOutput := sandbox.MustRunGit("log", "--oneline", "-n", "2")
	if !strings.Contains(logOutput, "watchflow: auto-commit reactive") {
		t.Errorf("commit automático não encontrado no git log. Histórico de commits:\n%s", logOutput)
	}

	_ = coord.Shutdown(context.Background())
}
