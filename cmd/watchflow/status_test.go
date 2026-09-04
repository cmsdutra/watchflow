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

func executeCommand(args ...string) (string, error) {
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
