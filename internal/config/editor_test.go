package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseConfigYAML(t *testing.T, tempDir, existingVaultPath string) string {
	t.Helper()
	return fmt.Sprintf(`# comentário do topo, deve sobreviver à edição
version: 1

daemon:
  state_dir: "%s/state"
  socket_path: "%s/sock"
  log_level: "info"

watchers:
  # comentário explicando o vault existente
  - name: "existente"
    path: "%s"
    debounce: "15s"
    max_wait: "60s"
    pull_interval: "5m"
    ignore:
      - ".git/**"
    pipelines:
      - "default"

pipelines:
  default:
    timeout: "120s"
    steps:
      - action: "git.add"
`, yamlPath(tempDir), yamlPath(tempDir), yamlPath(existingVaultPath))
}

func TestAddWatcherToFilePreservesCommentsAndAppends(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	newVault := filepath.Join(tempDir, "novo")
	for _, dir := range []string{existingVault, newVault} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfigYAML(t, tempDir, existingVault)), 0644); err != nil {
		t.Fatal(err)
	}

	err := AddWatcherToFile(cfgPath, WatcherConfig{
		Name: "novo",
		Path: newVault,
	})
	if err != nil {
		t.Fatalf("falha ao adicionar watcher: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	if !strings.Contains(out, "comentário do topo") {
		t.Error("comentário do topo do arquivo foi perdido")
	}
	if !strings.Contains(out, "comentário explicando o vault existente") {
		t.Error("comentário do watcher existente foi perdido")
	}
	if !strings.Contains(out, `name: "novo"`) {
		t.Errorf("watcher novo não foi anexado:\n%s", out)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("configuração resultante é inválida: %v", err)
	}
	if len(cfg.Watchers) != 2 {
		t.Fatalf("esperava 2 watchers, obteve %d", len(cfg.Watchers))
	}
}

// TestAddWatcherToFileInfersSolePipeline cobre o caso real do config.yaml de
// produção: um único pipeline nomeado 'vault-git-sync' (não 'default'). Sem
// --pipeline explícito, o comando deve inferir esse nome em vez de escrever
// uma referência a um pipeline 'default' que não existe no arquivo.
func TestAddWatcherToFileInfersSolePipeline(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	newVault := filepath.Join(tempDir, "novo")
	for _, dir := range []string{existingVault, newVault} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	content := fmt.Sprintf(`version: 1
daemon:
  state_dir: "%s/state"
  socket_path: "%s/sock"
  log_level: "info"
watchers:
  - name: "existente"
    path: "%s"
    debounce: "15s"
    max_wait: "60s"
    pipelines:
      - "vault-git-sync"
pipelines:
  vault-git-sync:
    timeout: "120s"
    steps:
      - action: "git.add"
`, yamlPath(tempDir), yamlPath(tempDir), yamlPath(existingVault))

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := AddWatcherToFile(cfgPath, WatcherConfig{Name: "novo", Path: newVault}); err != nil {
		t.Fatalf("falha ao adicionar watcher sem --pipeline explícito: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("configuração resultante é inválida: %v", err)
	}
	for _, w := range cfg.Watchers {
		if w.Name == "novo" {
			if len(w.Pipelines) != 1 || w.Pipelines[0] != "vault-git-sync" {
				t.Errorf("esperava pipeline inferido 'vault-git-sync', obteve %v", w.Pipelines)
			}
			return
		}
	}
	t.Fatal("watcher 'novo' não encontrado após adição")
}

func TestAddWatcherToFileRequiresPipelineWhenAmbiguous(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	newVault := filepath.Join(tempDir, "novo")
	for _, dir := range []string{existingVault, newVault} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	content := fmt.Sprintf(`version: 1
daemon:
  state_dir: "%s/state"
  socket_path: "%s/sock"
  log_level: "info"
watchers:
  - name: "existente"
    path: "%s"
    debounce: "15s"
    max_wait: "60s"
    pipelines:
      - "pipeline-a"
      - "pipeline-b"
pipelines:
  pipeline-a:
    timeout: "120s"
    steps:
      - action: "git.add"
  pipeline-b:
    timeout: "120s"
    steps:
      - action: "git.add"
`, yamlPath(tempDir), yamlPath(tempDir), yamlPath(existingVault))

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	err := AddWatcherToFile(cfgPath, WatcherConfig{Name: "novo", Path: newVault})
	if err == nil {
		t.Fatal("esperava erro pedindo --pipeline explícito quando há mais de um disponível")
	}
}

func TestAddWatcherToFileRejectsDuplicateName(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	if err := os.MkdirAll(existingVault, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfigYAML(t, tempDir, existingVault)), 0644); err != nil {
		t.Fatal(err)
	}

	err := AddWatcherToFile(cfgPath, WatcherConfig{Name: "existente", Path: existingVault})
	if err == nil {
		t.Fatal("esperava erro ao duplicar nome de watcher")
	}
}

func TestAddWatcherToFileRejectsMissingPath(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	if err := os.MkdirAll(existingVault, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfigYAML(t, tempDir, existingVault)), 0644); err != nil {
		t.Fatal(err)
	}

	err := AddWatcherToFile(cfgPath, WatcherConfig{Name: "fantasma", Path: filepath.Join(tempDir, "nao-existe")})
	if err == nil {
		t.Fatal("esperava erro ao apontar para caminho inexistente")
	}

	// O arquivo não deve ter sido alterado por uma edição que falhou na validação.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fantasma") {
		t.Error("arquivo foi alterado mesmo com validação falhando")
	}
}

func TestRemoveWatcherFromFilePreservesCommentsAndRemoves(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	otherVault := filepath.Join(tempDir, "outro")
	for _, dir := range []string{existingVault, otherVault} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	content := baseConfigYAML(t, tempDir, existingVault)
	// Insere um segundo watcher para que a remoção não deixe a lista vazia.
	content = strings.Replace(content, "pipelines:\n\ndefault:", "pipelines:\n\ndefault:", 1)
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := AddWatcherToFile(cfgPath, WatcherConfig{Name: "outro", Path: otherVault}); err != nil {
		t.Fatalf("setup: falha ao adicionar segundo watcher: %v", err)
	}

	if err := RemoveWatcherFromFile(cfgPath, "outro"); err != nil {
		t.Fatalf("falha ao remover watcher: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	if !strings.Contains(out, "comentário do topo") {
		t.Error("comentário do topo do arquivo foi perdido")
	}
	if !strings.Contains(out, "comentário explicando o vault existente") {
		t.Error("comentário do watcher remanescente foi perdido")
	}
	if strings.Contains(out, `name: "outro"`) {
		t.Errorf("watcher removido ainda presente:\n%s", out)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("configuração resultante é inválida: %v", err)
	}
	if len(cfg.Watchers) != 1 {
		t.Fatalf("esperava 1 watcher restante, obteve %d", len(cfg.Watchers))
	}
}

func TestRemoveWatcherFromFileUnknownName(t *testing.T) {
	tempDir := t.TempDir()
	existingVault := filepath.Join(tempDir, "existente")
	if err := os.MkdirAll(existingVault, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(baseConfigYAML(t, tempDir, existingVault)), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveWatcherFromFile(cfgPath, "nao-existe"); err == nil {
		t.Fatal("esperava erro ao remover watcher inexistente")
	}
}
