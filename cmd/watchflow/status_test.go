package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/watchflow/watchflow/internal/ipc"
)

type cliMockHandler struct{}

func (m *cliMockHandler) Status(ctx context.Context) (*ipc.StatusResponse, error) {
	return &ipc.StatusResponse{
		DaemonPID:   9876,
		Uptime:      "42m",
		Version:     "0.1.0-test",
		PendingJobs: 1,
		RunningJobs: 0,
		BlockedJobs: 0,
		Watchers: []ipc.WatcherStatusDTO{
			{
				ID:                "w-test",
				Name:              "obsidian-vault",
				Path:              "/vault",
				Status:            "HEALTHY",
				LastEventAt:       "2026-09-04 19:30",
				LastSuccessSyncAt: "2026-09-04 19:30",
				LastFailedSyncAt:  "-",
				LastError:         "",
			},
		},
	}, nil
}

func (m *cliMockHandler) Jobs(_ context.Context, _ string, _ int) (*ipc.JobsResponse, error) {
	return &ipc.JobsResponse{Jobs: []ipc.JobDTO{
		{ID: "job_1788000000000_vault_sync", WatcherID: "obsidian-vault", PipelineName: "vault-sync",
			Status: "PENDING", Files: 3, RetryCount: 1, MaxRetries: 5, ScheduledFor: "2026-09-04 10:00:05"},
		{ID: "job_1788000000001_vault_sync", WatcherID: "obsidian-vault", PipelineName: "vault-sync",
			Status: "BLOCKED", Files: 2, RetryCount: 0, MaxRetries: 5, LastError: "conflito de merge detectado"},
	}}, nil
}

func (m *cliMockHandler) Runs(_ context.Context, _ string, _ int) (*ipc.RunsResponse, error) {
	return &ipc.RunsResponse{Runs: []ipc.RunDTO{
		{ID: "run_1", JobID: "job_1", WatcherID: "obsidian-vault", PipelineName: "vault-sync",
			Status: "SUCCESS", DurationMs: 1250, CreatedAt: "2026-09-04 10:00:00"},
		{ID: "run_2", JobID: "job_2", WatcherID: "obsidian-vault", PipelineName: "vault-sync",
			Status: "FAILED", DurationMs: 300, ErrorStep: "git.push",
			ErrorDetails: "could not resolve host", CreatedAt: "2026-09-04 10:01:00"},
	}}, nil
}

func (m *cliMockHandler) Reload(_ context.Context) (*ipc.ReloadResponse, error) {
	return &ipc.ReloadResponse{
		Success: true, Message: "configuração recarregada",
		WatchersAdded: []string{"novo-vault"}, PipelinesTotal: 1,
	}, nil
}

func (m *cliMockHandler) Sync(ctx context.Context, watcherName string) (*ipc.SyncResponse, error) {
	return &ipc.SyncResponse{
		EnqueuedJobs: []string{"sync-job-1"},
		Message:      fmt.Sprintf("sincronização disparada para '%s'", watcherName),
	}, nil
}

func (m *cliMockHandler) Pause(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	return &ipc.ActionResponse{Success: true, Message: fmt.Sprintf("watcher '%s' pausado", watcherName)}, nil
}

func (m *cliMockHandler) Resume(ctx context.Context, watcherName string) (*ipc.ActionResponse, error) {
	return &ipc.ActionResponse{Success: true, Message: fmt.Sprintf("watcher '%s' retomado", watcherName)}, nil
}

func (m *cliMockHandler) Stop(ctx context.Context) (*ipc.ActionResponse, error) {
	return &ipc.ActionResponse{Success: true, Message: "desligamento iniciado"}, nil
}

func setupMockIPCServer(t *testing.T) string {
	t.Helper()
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "cli_test.sock")

	srv := ipc.NewServer(sockPath, &cliMockHandler{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	go func() {
		_ = srv.Start(ctx)
	}()

	client := ipc.NewClient(sockPath)
	for i := 0; i < 20; i++ {
		if client.IsDaemonRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	return sockPath
}

// resetCommandFlags devolve todas as flags aos valores padrão. O rootCmd é uma
// variável global compartilhada: sem isso, uma flag ligada por um teste (ex.:
// --json) vaza para os testes seguintes e para execuções repetidas com -count>1.
func resetCommandFlags(cmd *cobra.Command) {
	reset := func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	cmd.Flags().VisitAll(reset)
	cmd.PersistentFlags().VisitAll(reset)

	for _, sub := range cmd.Commands() {
		resetCommandFlags(sub)
	}
}

func executeCommand(args ...string) (string, error) {
	resetCommandFlags(rootCmd)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(args)

	// Redireciona temporariamente stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := rootCmd.Execute()

	_ = w.Close()
	os.Stdout = oldStdout
	capturedBuf := new(bytes.Buffer)
	_, _ = capturedBuf.ReadFrom(r)

	output := buf.String() + capturedBuf.String()
	return output, err
}

func TestCLI_Status(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	output, err := executeCommand("status", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro ao executar watchflow status: %v, output: %s", err, output)
	}

	if !strings.Contains(output, "obsidian-vault") {
		t.Errorf("esperava 'obsidian-vault' na saída do status: %s", output)
	}
	if !strings.Contains(output, "HEALTHY") {
		t.Errorf("esperava 'HEALTHY' na saída do status: %s", output)
	}
	if !strings.Contains(output, "PID: 9876") {
		t.Errorf("esperava PID na saída do status: %s", output)
	}
}

func TestCLI_StatusJSON(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	output, err := executeCommand("status", "--socket", sockPath, "--json")
	if err != nil {
		t.Fatalf("erro ao executar watchflow status --json: %v, output: %s", err, output)
	}

	if !strings.Contains(output, `"daemon_pid": 9876`) {
		t.Errorf("esperava JSON com daemon_pid: %s", output)
	}
}

func TestCLI_Sync(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	output, err := executeCommand("sync", "obsidian-vault", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro ao executar watchflow sync: %v", err)
	}

	if !strings.Contains(output, "sincronização disparada") {
		t.Errorf("esperava confirmação de sincronização: %s", output)
	}
}

func TestCLI_PauseAndResume(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	pauseOut, err := executeCommand("pause", "obsidian-vault", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro no pause: %v", err)
	}
	if !strings.Contains(pauseOut, "pausado") {
		t.Errorf("esperava confirmação de pause: %s", pauseOut)
	}

	resumeOut, err := executeCommand("resume", "obsidian-vault", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro no resume: %v", err)
	}
	if !strings.Contains(resumeOut, "retomado") {
		t.Errorf("esperava confirmação de resume: %s", resumeOut)
	}
}

func TestCLI_Stop(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	stopOut, err := executeCommand("stop", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro no stop: %v", err)
	}
	if !strings.Contains(stopOut, "desligamento iniciado") {
		t.Errorf("esperava confirmação de stop: %s", stopOut)
	}
}
