package ipc_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/ipc"
)

type mockHandler struct {
	statusFunc func(ctx context.Context) (*ipc.StatusResponse, error)
	syncFunc   func(ctx context.Context, watcherName string) (*ipc.SyncResponse, error)
	pauseFunc  func(ctx context.Context, watcherName string) (*ipc.ActionResponse, error)
	resumeFunc func(ctx context.Context, watcherName string) (*ipc.ActionResponse, error)
	stopFunc   func(ctx context.Context) (*ipc.ActionResponse, error)
}

func (m *mockHandler) Status(ctx context.Context) (*ipc.StatusResponse, error) {
	if m.statusFunc != nil {
		return m.statusFunc(ctx)
	}
	return &ipc.StatusResponse{
		DaemonPID: 1234,
		Uptime:    "1h20m",
		Version:   "1.0.0-test",
		Watchers: []ipc.WatcherStatusDTO{
			{Name: "my-vault", Path: "/tmp/vault", Status: "HEALTHY"},
		},
		PendingJobs: 0,
		RunningJobs: 1,
	}, nil
}

func (m *mockHandler) Jobs(_ context.Context, watcherName string, limit int) (*ipc.JobsResponse, error) {
	return &ipc.JobsResponse{Jobs: []ipc.JobDTO{{
		ID: "job_1", WatcherID: "vault", PipelineName: "sync", Status: "PENDING",
		Files: 3, RetryCount: 1, MaxRetries: 5,
	}}}, nil
}

func (m *mockHandler) Runs(_ context.Context, watcherName string, limit int) (*ipc.RunsResponse, error) {
	return &ipc.RunsResponse{Runs: []ipc.RunDTO{{
		ID: "run_1", JobID: "job_1", WatcherID: "vault", PipelineName: "sync",
		Status: "SUCCESS", DurationMs: 120, CreatedAt: "2026-09-04 10:00:00",
	}}}, nil
}

func (m *mockHandler) Sync(ctx context.Context, watcherName string) (*ipc.SyncResponse, error) {
	if m.syncFunc != nil {
		return m.syncFunc(ctx, watcherName)
	}
	return &ipc.SyncResponse{
		EnqueuedJobs: []string{"job-sync-1"},
		Message:      fmt.Sprintf("sync disparado para '%s'", watcherName),
	}, nil
}

func (m *mockHandler) Pause(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	if m.pauseFunc != nil {
		return m.pauseFunc(ctx, watcherName)
	}
	return &ipc.ActionResponse{Success: true, Message: "pausado"}, nil
}

func (m *mockHandler) Resume(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	if m.resumeFunc != nil {
		return m.resumeFunc(ctx, watcherName)
	}
	return &ipc.ActionResponse{Success: true, Message: "retomado"}, nil
}

func (m *mockHandler) Stop(ctx context.Context) (*ipc.ActionResponse, error) {
	if m.stopFunc != nil {
		return m.stopFunc(ctx)
	}
	return &ipc.ActionResponse{Success: true, Message: "desligando"}, nil
}

func TestIPC_RoundTrip(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "watchflow.sock")

	handler := &mockHandler{}
	srv := ipc.NewServer(sockPath, handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Aguarda o socket ser criado
	client := ipc.NewClient(sockPath)
	client.SetTimeout(2 * time.Second)

	var running bool
	for i := 0; i < 20; i++ {
		if client.IsDaemonRunning() {
			running = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !running {
		t.Fatalf("servidor IPC não iniciou a tempo")
	}

	// 1. Status
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("erro ao chamar status: %v", err)
	}
	if status.DaemonPID != 1234 || len(status.Watchers) != 1 {
		t.Errorf("resposta inesperada de status: %+v", status)
	}

	// 2. Sync
	syncResp, err := client.Sync(context.Background(), "my-vault")
	if err != nil {
		t.Fatalf("erro ao chamar sync: %v", err)
	}
	if len(syncResp.EnqueuedJobs) != 1 || syncResp.EnqueuedJobs[0] != "job-sync-1" {
		t.Errorf("resposta inesperada de sync: %+v", syncResp)
	}

	// 3. Pause
	pauseResp, err := client.Pause(context.Background(), "my-vault")
	if err != nil || !pauseResp.Success {
		t.Fatalf("erro ao chamar pause: %v, %+v", err, pauseResp)
	}

	// 4. Resume
	resumeResp, err := client.Resume(context.Background(), "my-vault")
	if err != nil || !resumeResp.Success {
		t.Fatalf("erro ao chamar resume: %v, %+v", err, resumeResp)
	}

	// 5. Stop
	stopResp, err := client.Stop(context.Background())
	if err != nil || !stopResp.Success {
		t.Fatalf("erro ao chamar stop: %v, %+v", err, stopResp)
	}

	// 6. Método desconhecido
	var dummy interface{}
	err = client.Call(context.Background(), "ghost_method", nil, &dummy)
	if err == nil {
		t.Errorf("esperava erro ao invocar método inexistente")
	}

	// Encerra servidor
	cancel()
	_ = srv.Close()
}

func TestIPC_ProcessLockConflict(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "watchflow.sock")

	handler := &mockHandler{}
	srv1 := ipc.NewServer(sockPath, handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv1.Start(ctx)
	}()

	client := ipc.NewClient(sockPath)
	for i := 0; i < 20; i++ {
		if client.IsDaemonRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Segunda instância tentando abrir no mesmo socket deve falhar
	srv2 := ipc.NewServer(sockPath, handler)
	err := srv2.Start(ctx)
	if err == nil {
		t.Fatalf("esperava erro de conflito de processo ao iniciar srv2 no mesmo socket")
	}

	_ = srv1.Close()
}

// TestIPC_JobsAndRunsRoundTrip garante que os métodos novos atravessam o
// protocolo JSON-RPC preservando os campos que a TUI vai consumir.
func TestIPC_JobsAndRunsRoundTrip(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "wf.sock")
	handler := &mockHandler{}

	srv := ipc.NewServer(sockPath, handler)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = srv.Close()
	}()

	go func() { _ = srv.Start(ctx) }()
	<-srv.Ready()

	client := ipc.NewClient(sockPath)

	jobs, err := client.Jobs(context.Background(), "vault", 10)
	if err != nil {
		t.Fatalf("chamada 'jobs' falhou: %v", err)
	}
	if len(jobs.Jobs) != 1 {
		t.Fatalf("esperava 1 job, obteve %d", len(jobs.Jobs))
	}
	if jobs.Jobs[0].Files != 3 || jobs.Jobs[0].RetryCount != 1 || jobs.Jobs[0].MaxRetries != 5 {
		t.Errorf("campos do job não sobreviveram ao round-trip: %+v", jobs.Jobs[0])
	}

	runs, err := client.Runs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("chamada 'runs' falhou: %v", err)
	}
	if len(runs.Runs) != 1 {
		t.Fatalf("esperava 1 execução, obteve %d", len(runs.Runs))
	}
	if runs.Runs[0].DurationMs != 120 || runs.Runs[0].JobID != "job_1" {
		t.Errorf("campos da execução não sobreviveram ao round-trip: %+v", runs.Runs[0])
	}
}

func TestIPC_UnknownMethodStillRejected(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "wf.sock")

	srv := ipc.NewServer(sockPath, &mockHandler{})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = srv.Close()
	}()

	go func() { _ = srv.Start(ctx) }()
	<-srv.Ready()

	client := ipc.NewClient(sockPath)
	var out map[string]any
	if err := client.Call(context.Background(), "metodo_inexistente", nil, &out); err == nil {
		t.Error("esperava erro para método desconhecido")
	}
}
