package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/watchflow/watchflow/internal/notify"
)

// Config representa a estrutura canônica de configuração do WatchFlow.
type Config struct {
	Version       int                 `yaml:"version"`
	Daemon        DaemonConfig        `yaml:"daemon"`
	Notifications NotificationConfig  `yaml:"notifications"`
	Watchers      []WatcherConfig     `yaml:"watchers"`
	Pipelines     map[string]Pipeline `yaml:"pipelines"`
}

// DaemonConfig define parâmetros operacionais do processo daemon.
type DaemonConfig struct {
	StateDir               string `yaml:"state_dir"`
	SocketPath             string `yaml:"socket_path"`
	LogLevel               string `yaml:"log_level"`
	MaxConcurrentPipelines int    `yaml:"max_concurrent_pipelines"`
}

// NotificationConfig define os canais e regras para despacho de alertas.
type NotificationConfig struct {
	Enabled    bool   `yaml:"enabled"`
	OnSuccess  bool   `yaml:"on_success"`
	OnError    bool   `yaml:"on_error"`
	OnConflict bool   `yaml:"on_conflict"`
	Backend    string `yaml:"backend"`

	// WebhookURL é obrigatório quando Backend == "webhook".
	WebhookURL string `yaml:"webhook_url,omitempty"`
}

// NotifyOptions converte a configuração declarada no YAML nas opções do
// subsistema de notificações.
func (n *NotificationConfig) NotifyOptions() notify.Options {
	return notify.Options{
		Enabled:    n.Enabled,
		OnSuccess:  n.OnSuccess,
		OnError:    n.OnError,
		OnConflict: n.OnConflict,
		Backend:    n.Backend,
		WebhookURL: n.WebhookURL,
	}
}

// WatcherConfig define o monitoramento para um diretório raiz específico.
type WatcherConfig struct {
	Name      string   `yaml:"name"`
	Path      string   `yaml:"path"`
	Enabled   *bool    `yaml:"enabled,omitempty"`
	Debounce  string   `yaml:"debounce"`
	MaxWait   string   `yaml:"max_wait"`
	Ignore    []string `yaml:"ignore"`
	Pipelines []string `yaml:"pipelines"`

	// Campos derivados (preenchidos após o parsing)
	DebounceDuration time.Duration `yaml:"-"`
	MaxWaitDuration  time.Duration `yaml:"-"`
	ResolvedPath     string        `yaml:"-"`
}

// IsEnabled retorna true se o watcher estiver ativo (padrão é true quando não especificado).
func (w *WatcherConfig) IsEnabled() bool {
	if w.Enabled == nil {
		return true
	}
	return *w.Enabled
}

// DefaultMaxRetries é o número de tentativas aplicado quando o pipeline não
// declara max_retries.
const DefaultMaxRetries = 5

// DefaultPipelineName é o nome do pipeline embutido, atribuído a watchers que
// não declaram nenhum.
const DefaultPipelineName = "default"

// DefaultPipeline devolve o pipeline de sincronização Git padrão.
//
// Os cinco passos, nesta ordem, são o que todo usuário escreveria de qualquer
// forma; deixá-los implícitos reduz a configuração mínima a duas linhas por
// pasta e tira do usuário a responsabilidade de saber que 'git.check_locks'
// precisa vir antes de 'git.add'.
func DefaultPipeline() Pipeline {
	return Pipeline{
		Timeout: "120s",
		Steps: []Step{
			{Action: "git.check_locks"},
			{Action: "git.add"},
			{Action: "git.commit"},
			{Action: "git.safe_sync"},
			{Action: "git.push"},
		},
	}
}

// Pipeline define a sequência ordenada de ações com timeout de execução.
type Pipeline struct {
	Timeout string `yaml:"timeout"`
	Steps   []Step `yaml:"steps"`

	// MaxRetries limita as tentativas de um job deste pipeline antes da falha
	// definitiva. Zero ou omitido usa DefaultMaxRetries.
	MaxRetries int `yaml:"max_retries,omitempty"`

	// Campos derivados
	TimeoutDuration time.Duration `yaml:"-"`
}

// RetryLimit retorna o número efetivo de tentativas do pipeline.
func (p *Pipeline) RetryLimit() int {
	if p.MaxRetries <= 0 {
		return DefaultMaxRetries
	}
	return p.MaxRetries
}

// Step representa uma ação atômica e seus parâmetros de execução dentro de um pipeline.
type Step struct {
	Action string                 `yaml:"action"`
	Params map[string]interface{} `yaml:"params"`
}

// DefaultConfigPath retorna o caminho padrão para o arquivo de configuração (~/.config/watchflow/config.yaml).
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/watchflow/config.yaml"
	}
	return filepath.Join(home, ".config", "watchflow", "config.yaml")
}

// ExpandPath expande o caractere '~', a variável ${UID} e variáveis de ambiente no caminho.
func ExpandPath(path string) string {
	if path == "" {
		return ""
	}

	// Expande ${UID} especificamente caso esteja no caminho
	uid := fmt.Sprintf("%d", os.Getuid())
	path = strings.ReplaceAll(path, "${UID}", uid)
	path = strings.ReplaceAll(path, "$UID", uid)

	// Expande outras variáveis de ambiente
	path = os.ExpandEnv(path)

	// Expande '~' para o diretório HOME
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				path = home
			} else if strings.HasPrefix(path, "~/") {
				path = filepath.Join(home, path[2:])
			}
		}
	}

	return filepath.Clean(path)
}

// applyDefaults preenche campos vazios com valores seguros predefinidos.
func applyDefaults(cfg *Config) {
	if cfg.Version == 0 {
		cfg.Version = 1
	}

	if cfg.Daemon.StateDir == "" {
		cfg.Daemon.StateDir = "~/.local/state/watchflow"
	}
	cfg.Daemon.StateDir = ExpandPath(cfg.Daemon.StateDir)

	if cfg.Daemon.SocketPath == "" {
		runUserDir := fmt.Sprintf("/run/user/%d", os.Getuid())
		if _, err := os.Stat(runUserDir); err == nil {
			cfg.Daemon.SocketPath = filepath.Join(runUserDir, "watchflow.sock")
		} else {
			cfg.Daemon.SocketPath = filepath.Join(cfg.Daemon.StateDir, "watchflow.sock")
		}
	} else {
		cfg.Daemon.SocketPath = ExpandPath(cfg.Daemon.SocketPath)
	}

	if cfg.Daemon.LogLevel == "" {
		cfg.Daemon.LogLevel = "info"
	}
	cfg.Daemon.LogLevel = strings.ToLower(cfg.Daemon.LogLevel)

	if cfg.Daemon.MaxConcurrentPipelines <= 0 {
		cfg.Daemon.MaxConcurrentPipelines = 2
	}

	if cfg.Notifications.Backend == "" {
		cfg.Notifications.Backend = "desktop"
	}
	cfg.Notifications.Backend = strings.ToLower(cfg.Notifications.Backend)

	usesDefaultPipeline := false
	for i := range cfg.Watchers {
		w := &cfg.Watchers[i]
		if w.Debounce == "" {
			w.Debounce = "15s"
		}
		if w.MaxWait == "" {
			w.MaxWait = "60s"
		}
		w.ResolvedPath = ExpandPath(w.Path)

		if len(w.Pipelines) == 0 {
			w.Pipelines = []string{DefaultPipelineName}
			usesDefaultPipeline = true
		}
	}

	// Só injeta o pipeline embutido se alguém realmente depender dele, e nunca
	// por cima de um pipeline que o usuário tenha definido com o mesmo nome.
	if usesDefaultPipeline {
		if cfg.Pipelines == nil {
			cfg.Pipelines = make(map[string]Pipeline)
		}
		if _, declared := cfg.Pipelines[DefaultPipelineName]; !declared {
			cfg.Pipelines[DefaultPipelineName] = DefaultPipeline()
		}
	}

	for name, p := range cfg.Pipelines {
		if p.Timeout == "" {
			p.Timeout = "120s"
		}
		cfg.Pipelines[name] = p
	}
}

// Load lê o arquivo YAML do caminho especificado, aplica defaults e valida as regras.
func Load(filePath string) (*Config, error) {
	expandedPath := ExpandPath(filePath)
	data, err := os.ReadFile(expandedPath)
	if err != nil {
		return nil, fmt.Errorf("falha ao ler arquivo de configuração '%s': %w", expandedPath, err)
	}

	return LoadBytes(data, true)
}

// LoadBytes processa o conteúdo YAML a partir de bytes em memória.
func LoadBytes(data []byte, checkPathsExist bool) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("sintaxe YAML inválida: %w", err)
	}

	applyDefaults(&cfg)

	if err := Validate(&cfg, checkPathsExist); err != nil {
		return nil, fmt.Errorf("configuração inválida: %w", err)
	}

	return &cfg, nil
}
