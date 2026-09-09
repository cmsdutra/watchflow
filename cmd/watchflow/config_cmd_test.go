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
`, yamlPath(tempDir), yamlPath(tempDir), yamlPath(vaultDir))

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

func writeBaseConfigForCmdTest(t *testing.T, cfgPath, tempDir, vaultDir string) {
	t.Helper()
	content := fmt.Sprintf(`version: 1
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
`, yamlPath(tempDir), yamlPath(tempDir), yamlPath(vaultDir))
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("falha ao gravar config.yaml: %v", err)
	}
}

func TestConfigAddWatcherCommand(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "vault")
	newVaultDir := filepath.Join(tempDir, "novo-vault")
	for _, dir := range []string{vaultDir, newVaultDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	writeBaseConfigForCmdTest(t, cfgPath, tempDir, vaultDir)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"config", "add-watcher", "novo", newVaultDir,
		"--config", cfgPath,
		"--pipeline", "git-sync",
		"--no-reload",
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("esperava sucesso ao adicionar watcher, obteve: %v", err)
	}

	if !bytes.Contains(buf.Bytes(), []byte("adicionado")) {
		t.Errorf("saída esperada não encontrada, obteve:\n%s", buf.String())
	}

	rootCmd.SetArgs([]string{"config", "validate", "--config", cfgPath})
	buf.Reset()
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("configuração deveria continuar válida após adicionar watcher: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Watchers configurados: 2")) {
		t.Errorf("watcher novo não foi persistido, obteve:\n%s", buf.String())
	}
}

func TestConfigAddWatcherCommandMissingPath(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "vault")
	if err := os.Mkdir(vaultDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	writeBaseConfigForCmdTest(t, cfgPath, tempDir, vaultDir)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"config", "add-watcher", "fantasma", filepath.Join(tempDir, "nao-existe"),
		"--config", cfgPath,
		"--no-reload",
	})

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("esperava erro ao apontar para caminho inexistente")
	}
}

func TestConfigRemoveWatcherCommand(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "vault")
	otherVaultDir := filepath.Join(tempDir, "outro-vault")
	for _, dir := range []string{vaultDir, otherVaultDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	writeBaseConfigForCmdTest(t, cfgPath, tempDir, vaultDir)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"config", "add-watcher", "outro", otherVaultDir,
		"--config", cfgPath,
		"--pipeline", "git-sync",
		"--no-reload",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("setup: falha ao adicionar segundo watcher: %v", err)
	}

	buf.Reset()
	rootCmd.SetArgs([]string{
		"config", "remove-watcher", "outro",
		"--config", cfgPath,
		"--no-reload",
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("esperava sucesso ao remover watcher, obteve: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("removido")) {
		t.Errorf("saída esperada não encontrada, obteve:\n%s", buf.String())
	}

	buf.Reset()
	rootCmd.SetArgs([]string{"config", "validate", "--config", cfgPath})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("configuração deveria continuar válida após remover watcher: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Watchers configurados: 1")) {
		t.Errorf("watcher não foi removido, obteve:\n%s", buf.String())
	}
}

func TestConfigRemoveWatcherCommandUnknownName(t *testing.T) {
	tempDir := t.TempDir()
	vaultDir := filepath.Join(tempDir, "vault")
	if err := os.Mkdir(vaultDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	writeBaseConfigForCmdTest(t, cfgPath, tempDir, vaultDir)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"config", "remove-watcher", "nao-existe",
		"--config", cfgPath,
		"--no-reload",
	})

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("esperava erro ao remover watcher inexistente")
	}
}
