package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValidYAML(t *testing.T) {
	tempDir := t.TempDir()

	yamlData := fmt.Sprintf(`
version: 1

daemon:
  state_dir: "%s/state"
  socket_path: "%s/watchflow.sock"
  log_level: "debug"
  max_concurrent_pipelines: 4

notifications:
  enabled: true
  backend: "desktop"

watchers:
  - name: "vault-1"
    path: "%s"
    debounce: "10s"
    max_wait: "30s"
    ignore:
      - "*.tmp"
    pipelines:
      - "sync-pipe"

pipelines:
  sync-pipe:
    timeout: "60s"
    steps:
      - action: "git.add"
      - action: "git.commit"
        params:
          message: "test commit"
`, tempDir, tempDir, tempDir)

	cfg, err := LoadBytes([]byte(yamlData), true)
	if err != nil {
		t.Fatalf("esperava sucesso ao carregar YAML válido, obteve: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("esperava versão 1, obteve %d", cfg.Version)
	}
	if cfg.Daemon.LogLevel != "debug" {
		t.Errorf("esperava log_level debug, obteve %s", cfg.Daemon.LogLevel)
	}
	if cfg.Daemon.MaxConcurrentPipelines != 4 {
		t.Errorf("esperava max_concurrent_pipelines 4, obteve %d", cfg.Daemon.MaxConcurrentPipelines)
	}
	if len(cfg.Watchers) != 1 {
		t.Fatalf("esperava 1 watcher, obteve %d", len(cfg.Watchers))
	}

	w := cfg.Watchers[0]
	if w.Name != "vault-1" {
		t.Errorf("esperava nome 'vault-1', obteve %s", w.Name)
	}
	if !w.IsEnabled() {
		t.Errorf("esperava watcher habilitado por padrão")
	}
	if w.DebounceDuration != 10*time.Second {
		t.Errorf("esperava debounce de 10s, obteve %v", w.DebounceDuration)
	}
	if w.MaxWaitDuration != 30*time.Second {
		t.Errorf("esperava max_wait de 30s, obteve %v", w.MaxWaitDuration)
	}

	pipe, exists := cfg.Pipelines["sync-pipe"]
	if !exists {
		t.Fatalf("pipeline 'sync-pipe' não encontrado")
	}
	if pipe.TimeoutDuration != 60*time.Second {
		t.Errorf("esperava timeout de 60s, obteve %v", pipe.TimeoutDuration)
	}
	if len(pipe.Steps) != 2 {
		t.Errorf("esperava 2 steps, obteve %d", len(pipe.Steps))
	}
}

func TestDefaultsApplication(t *testing.T) {
	tempDir := t.TempDir()

	minimalYAML := fmt.Sprintf(`
watchers:
  - name: "minimal-vault"
    path: "%s"
    pipelines:
      - "pipe"

pipelines:
  pipe:
    steps:
      - action: "noop"
`, tempDir)

	cfg, err := LoadBytes([]byte(minimalYAML), true)
	if err != nil {
		t.Fatalf("esperava sucesso com defaults aplicados, obteve: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("default de versão deve ser 1, obteve %d", cfg.Version)
	}
	if cfg.Daemon.LogLevel != "info" {
		t.Errorf("default de log_level deve ser 'info', obteve %s", cfg.Daemon.LogLevel)
	}
	if cfg.Daemon.MaxConcurrentPipelines != 2 {
		t.Errorf("default de max_concurrent_pipelines deve ser 2, obteve %d", cfg.Daemon.MaxConcurrentPipelines)
	}
	if cfg.Notifications.Backend != "desktop" {
		t.Errorf("default de notifications backend deve ser 'desktop', obteve %s", cfg.Notifications.Backend)
	}
	if cfg.Watchers[0].DebounceDuration != 15*time.Second {
		t.Errorf("default de debounce deve ser 15s, obteve %v", cfg.Watchers[0].DebounceDuration)
	}
	if cfg.Watchers[0].MaxWaitDuration != 60*time.Second {
		t.Errorf("default de max_wait deve ser 60s, obteve %v", cfg.Watchers[0].MaxWaitDuration)
	}
	if cfg.Pipelines["pipe"].TimeoutDuration != 120*time.Second {
		t.Errorf("default de timeout de pipeline deve ser 120s, obteve %v", cfg.Pipelines["pipe"].TimeoutDuration)
	}
}

func TestValidationErrors(t *testing.T) {
	tempDir := t.TempDir()

	testCases := []struct {
		name        string
		yaml        string
		checkPaths  bool
		expectedErr string
	}{
		{
			name:        "yaml_truncado",
			yaml:        "watchers: [",
			expectedErr: "sintaxe YAML inválida",
		},
		{
			name: "versao_invalida",
			yaml: fmt.Sprintf(`
version: 2
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "versão de configuração não suportada: 2",
		},
		{
			name: "log_level_invalido",
			yaml: fmt.Sprintf(`
daemon:
  log_level: "verbose"
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "log_level 'verbose' inválido",
		},
		{
			name: "backend_notificacao_invalido",
			yaml: fmt.Sprintf(`
notifications:
  backend: "telegram"
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "backend 'telegram' inválido",
		},
		{
			name: "sem_watchers",
			yaml: `
pipelines:
  p:
    steps: [{action: "a"}]
`,
			expectedErr: "nenhum watcher foi declarado",
		},
		{
			name: "watcher_sem_nome",
			yaml: fmt.Sprintf(`
watchers:
  - path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "campo 'name' é obrigatório",
		},
		{
			name: "watcher_duplicado",
			yaml: fmt.Sprintf(`
watchers:
  - name: "dup"
    path: "%s"
    pipelines: ["p"]
  - name: "dup"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir, tempDir),
			expectedErr: "watcher duplicado com o nome 'dup'",
		},
		{
			name: "caminho_inexistente",
			yaml: `
watchers:
  - name: "v"
    path: "/caminho/completamente/inexistente/no/sistema"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`,
			checkPaths:  true,
			expectedErr: "não existe",
		},
		{
			name: "debounce_invalido",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    debounce: "invalid-time"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "valor de debounce 'invalid-time' inválido",
		},
		{
			name: "debounce_zero",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    debounce: "0s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "debounce deve ser maior que zero",
		},
		{
			name: "max_wait_menor_que_debounce",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    debounce: "30s"
    max_wait: "10s"
    pipelines: ["p"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "max_wait (10s) não pode ser menor que debounce (30s)",
		},
		{
			name: "pipeline_inexistente_referenciado",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["pipeline-fantasma"]
pipelines:
  p:
    steps: [{action: "a"}]
`, tempDir),
			expectedErr: "referencia o pipeline inexistente 'pipeline-fantasma'",
		},
		{
			name: "sem_pipelines",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
`, tempDir),
			expectedErr: "nenhum pipeline foi declarado",
		},
		{
			name: "pipeline_sem_steps",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps: []
`, tempDir),
			expectedErr: "pipeline 'p': deve conter ao menos um step",
		},
		{
			name: "step_sem_action",
			yaml: fmt.Sprintf(`
watchers:
  - name: "v"
    path: "%s"
    pipelines: ["p"]
pipelines:
  p:
    steps:
      - params: {key: "val"}
`, tempDir),
			expectedErr: "step 1: campo 'action' é obrigatório",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.yaml), tc.checkPaths)
			if err == nil {
				t.Fatalf("esperava erro contendo '%s', mas obteve sucesso", tc.expectedErr)
			}
			if tc.expectedErr != "" && !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("esperava erro contendo '%s', obteve: %v", tc.expectedErr, err)
			}
			t.Logf("Erro capturado com sucesso: %v", err)
		})
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	uid := fmt.Sprintf("%d", os.Getuid())

	if got := ExpandPath("~/meu-vault"); got != filepath.Join(home, "meu-vault") {
		t.Errorf("esperava %s, obteve %s", filepath.Join(home, "meu-vault"), got)
	}

	if got := ExpandPath("/run/user/${UID}/watchflow.sock"); got != fmt.Sprintf("/run/user/%s/watchflow.sock", uid) {
		t.Errorf("esperava /run/user/%s/watchflow.sock, obteve %s", uid, got)
	}

	if got := ExpandPath(""); got != "" {
		t.Errorf("esperava string vazia, obteve %s", got)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	p := DefaultConfigPath()
	if !filepath.IsAbs(p) {
		t.Errorf("DefaultConfigPath deve ser absoluto, obteve: %s", p)
	}
	if filepath.Base(p) != "config.yaml" {
		t.Errorf("esperava nome de arquivo config.yaml, obteve: %s", filepath.Base(p))
	}
}

func TestLoadFromFile(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "config.yaml")

	content := fmt.Sprintf(`
version: 1
watchers:
  - name: "file-vault"
    path: "%s"
    pipelines: ["pipe"]
pipelines:
  pipe:
    steps:
      - action: "git.status"
`, tempDir)

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("falha ao gravar arquivo temporário: %v", err)
	}

	cfg, err := Load(filePath)
	if err != nil {
		t.Fatalf("Load falhou: %v", err)
	}
	if cfg.Watchers[0].Name != "file-vault" {
		t.Errorf("esperava 'file-vault', obteve: %s", cfg.Watchers[0].Name)
	}

	// Teste com arquivo inexistente
	_, err = Load(filepath.Join(tempDir, "nao_existe.yaml"))
	if err == nil {
		t.Errorf("esperava erro ao tentar carregar arquivo inexistente")
	}
}

func TestWatcherExplicitlyDisabled(t *testing.T) {
	tempDir := t.TempDir()
	content := fmt.Sprintf(`
watchers:
  - name: "disabled-vault"
    path: "%s"
    enabled: false
    pipelines: ["pipe"]
pipelines:
  pipe:
    steps: [{action: "test"}]
`, tempDir)

	cfg, err := LoadBytes([]byte(content), false)
	if err != nil {
		t.Fatalf("falha ao carregar config: %v", err)
	}
	if cfg.Watchers[0].IsEnabled() {
		t.Errorf("esperava watcher desabilitado")
	}
}

func TestValidateNilConfig(t *testing.T) {
	if err := Validate(nil, false); err == nil {
		t.Errorf("esperava erro ao validar config nil")
	}
}
