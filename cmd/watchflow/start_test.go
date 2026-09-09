package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/ipc"
)

func TestStartCmd_Flags(t *testing.T) {
	flag := startCmd.Flags().Lookup("foreground")
	if flag == nil {
		t.Fatal("flag --foreground não encontrada em startCmd")
	}
	if flag.Shorthand != "f" {
		t.Errorf("shorthand esperado 'f', obtido '%s'", flag.Shorthand)
	}
}

func TestStartCmd_InvalidConfigFile(t *testing.T) {
	cfgFile = "/caminho/inexistente/config.yaml"
	socketFlag = ""
	defer func() {
		cfgFile = ""
		socketFlag = ""
	}()

	buf := new(bytes.Buffer)
	cmd := startCmd
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	err := runStart(cmd, []string{})
	if err == nil {
		t.Fatal("esperava erro ao tentar iniciar com arquivo inexistente, mas retornou sucesso")
	}
}

func TestStartCmd_LifecycleWithStopIPC(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "test_vault")
	if err := os.MkdirAll(vaultDir, 0755); err != nil {
		t.Fatalf("falha ao criar pasta de teste: %v", err)
	}

	sockPath := filepath.Join(tempDir, "watchflow.sock")
	stateDir := filepath.Join(tempDir, "state")
	cfgPath := filepath.Join(tempDir, "watchflow.yaml")

	cfgContent := `version: 1
daemon:
  state_dir: "` + yamlPath(stateDir) + `"
  socket_path: "` + yamlPath(sockPath) + `"
  log_level: "info"
  max_concurrent_pipelines: 1
watchers:
  - name: "start-test-vault"
    path: "` + yamlPath(vaultDir) + `"
    debounce: "50ms"
    max_wait: "100ms"
    pipelines:
      - "test-pipe"
pipelines:
  test-pipe:
    timeout: "10s"
    steps:
      - action: "git.check_locks"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("falha ao escrever config: %v", err)
	}

	startDone := make(chan error, 1)
	go func() {
		buf := new(bytes.Buffer)
		cmd := startCmd
		cmd.SetOut(buf)
		cmd.SetErr(buf)

		// Executa runStart diretamente
		cfgFile = cfgPath
		socketFlag = sockPath
		startDone <- runStart(cmd, []string{})
	}()

	// Aguarda startup do socket Unix
	var client *ipc.Client
	var connected bool
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(sockPath); err == nil {
			client = ipc.NewClient(sockPath)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			if _, err := client.Status(ctx); err == nil {
				connected = true
				cancel()
				break
			}
			cancel()
		}
	}

	if !connected {
		t.Fatalf("daemon não iniciou o socket Unix em tempo hábil")
	}

	// Solicita encerramento via Stop IPC
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stopResp, err := client.Stop(ctx)
	if err != nil || !stopResp.Success {
		t.Fatalf("falha ao solicitar parada via IPC: %v", err)
	}

	// Aguarda o encerramento limpo da goroutine do start
	select {
	case err := <-startDone:
		if err != nil {
			t.Errorf("runStart retornou erro inesperado no shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runStart não finalizou dentro do tempo limite após comando stop")
	}
}
