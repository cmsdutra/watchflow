package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValidateCommand(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "vault")
	if err := os.Mkdir(vaultDir, 0755); err != nil {
		t.Fatalf("falha ao criar vaultDir: %v", err)
	}

	validConfigPath := filepath.Join(tempDir, "valid.yaml")
	validContent := fmt.Sprintf(`
version: 1
daemon:
  state_dir: "%s/state"
  socket_path: "%s/sock"
  log_level: "info"
watchers:
  - name: "test-vault"
    path: "%s"
    debounce: "5s"
    max_wait: "20s"
    pipelines: ["git-sync"]
pipelines:
  git-sync:
    timeout: "30s"
    steps:
      - action: "git.add"
`, tempDir, tempDir, vaultDir)

	if err := os.WriteFile(validConfigPath, []byte(validContent), 0644); err != nil {
		t.Fatalf("falha ao gravar valid.yaml: %v", err)
	}

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"config", "validate", "--config", validConfigPath})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("esperava sucesso ao validar config válida, obteve: %v", err)
	}

	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("Configuração válida")) {
		t.Errorf("saída esperada não encontrada, obteve:\n%s", output)
	}
}

func TestConfigValidateCommandFileNotFound(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"config", "validate", "--config", "/caminho/arquivo/nao_existe.yaml"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatalf("esperava erro ao apontar para arquivo inexistente")
	}
}
