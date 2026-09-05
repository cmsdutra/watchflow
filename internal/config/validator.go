package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/notify"
	"github.com/watchflow/watchflow/internal/providers"
)

var (
	validLogLevels = map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}

	validNotificationBackends = map[string]bool{
		"desktop": true,
		"log":     true,
		"webhook": true,
	}
)

// Validate verifica a consistência semântica de toda a árvore de configuração.
func Validate(cfg *Config, checkPathsExist bool) error {
	if cfg == nil {
		return fmt.Errorf("configuração não pode ser nula")
	}

	if cfg.Version != 1 {
		return fmt.Errorf("versão de configuração não suportada: %d (esperada: 1)", cfg.Version)
	}

	if err := validateDaemon(&cfg.Daemon); err != nil {
		return fmt.Errorf("seção 'daemon' inválida: %w", err)
	}

	if err := validateNotifications(&cfg.Notifications); err != nil {
		return fmt.Errorf("seção 'notifications' inválida: %w", err)
	}

	if err := validatePipelines(cfg.Pipelines); err != nil {
		return fmt.Errorf("seção 'pipelines' inválida: %w", err)
	}

	if err := validateWatchers(cfg.Watchers, cfg.Pipelines, checkPathsExist); err != nil {
		return fmt.Errorf("seção 'watchers' inválida: %w", err)
	}

	return nil
}

func validateDaemon(d *DaemonConfig) error {
	if d.StateDir == "" {
		return fmt.Errorf("campo 'state_dir' é obrigatório")
	}

	if d.SocketPath == "" {
		return fmt.Errorf("campo 'socket_path' é obrigatório")
	}

	if !validLogLevels[d.LogLevel] {
		return fmt.Errorf("log_level '%s' inválido; use: debug, info, warn, error", d.LogLevel)
	}

	if d.MaxConcurrentPipelines <= 0 {
		return fmt.Errorf("max_concurrent_pipelines deve ser maior que 0 (recebido: %d)", d.MaxConcurrentPipelines)
	}

	return nil
}

func validateNotifications(n *NotificationConfig) error {
	if !validNotificationBackends[n.Backend] {
		return fmt.Errorf("backend '%s' inválido; use: desktop, log, webhook", n.Backend)
	}

	// Só exige a URL se o backend for de fato usado, para não reprovar
	// configurações com notificações desligadas.
	if n.Enabled && n.Backend == "webhook" {
		if err := notify.ValidateWebhookURL(n.WebhookURL); err != nil {
			return err
		}
	}

	return nil
}

func validatePipelines(pipelines map[string]Pipeline) error {
	if len(pipelines) == 0 {
		return fmt.Errorf("nenhum pipeline foi declarado")
	}

	for name, p := range pipelines {
		if name == "" {
			return fmt.Errorf("nome do pipeline não pode ser vazio")
		}

		timeoutDur, err := time.ParseDuration(p.Timeout)
		if err != nil {
			return fmt.Errorf("pipeline '%s': timeout '%s' inválido: %w", name, p.Timeout, err)
		}
		if timeoutDur <= 0 {
			return fmt.Errorf("pipeline '%s': timeout deve ser maior que zero", name)
		}

		p.TimeoutDuration = timeoutDur

		if p.MaxRetries < 0 {
			return fmt.Errorf("pipeline '%s': max_retries não pode ser negativo (recebido: %d)", name, p.MaxRetries)
		}

		if len(p.Steps) == 0 {
			return fmt.Errorf("pipeline '%s': deve conter ao menos um step", name)
		}

		for idx, step := range p.Steps {
			if step.Action == "" {
				return fmt.Errorf("pipeline '%s', step %d: campo 'action' é obrigatório", name, idx+1)
			}

			// Um typo em 'action' passava na validação e só falhava em runtime
			// como erro fatal, com o job já enfileirado. O catálogo só está
			// populado quando os providers foram ligados ao binário; se estiver
			// vazio a checagem é pulada em vez de reprovar indevidamente.
			if known := providers.DefaultRegistry.List(); len(known) > 0 {
				if _, exists := providers.DefaultRegistry.Get(step.Action); !exists {
					return fmt.Errorf("pipeline '%s', step %d: ação '%s' não existe; disponíveis: %s",
						name, idx+1, step.Action, strings.Join(known, ", "))
				}

				action, _ := providers.DefaultRegistry.Get(step.Action)
				if err := action.Validate(step.Params); err != nil {
					return fmt.Errorf("pipeline '%s', step %d (%s): %w", name, idx+1, step.Action, err)
				}
			}
		}

		pipelines[name] = p
	}

	return nil
}

func validateWatchers(watchers []WatcherConfig, pipelines map[string]Pipeline, checkPathsExist bool) error {
	if len(watchers) == 0 {
		return fmt.Errorf("nenhum watcher foi declarado")
	}

	seenNames := make(map[string]bool)

	for i := range watchers {
		w := &watchers[i]

		if w.Name == "" {
			return fmt.Errorf("watcher no índice %d: campo 'name' é obrigatório", i)
		}

		if seenNames[w.Name] {
			return fmt.Errorf("watcher duplicado com o nome '%s'", w.Name)
		}
		seenNames[w.Name] = true

		if w.Path == "" {
			return fmt.Errorf("watcher '%s': campo 'path' é obrigatório", w.Name)
		}

		if checkPathsExist {
			stat, err := os.Stat(w.ResolvedPath)
			if err != nil {
				return fmt.Errorf("watcher '%s': caminho '%s' não existe: %w", w.Name, w.ResolvedPath, err)
			}
			if !stat.IsDir() {
				return fmt.Errorf("watcher '%s': caminho '%s' não é um diretório", w.Name, w.ResolvedPath)
			}
		}

		debounceDur, err := time.ParseDuration(w.Debounce)
		if err != nil {
			return fmt.Errorf("watcher '%s': valor de debounce '%s' inválido: %w", w.Name, w.Debounce, err)
		}
		if debounceDur <= 0 {
			return fmt.Errorf("watcher '%s': debounce deve ser maior que zero", w.Name)
		}
		w.DebounceDuration = debounceDur

		maxWaitDur, err := time.ParseDuration(w.MaxWait)
		if err != nil {
			return fmt.Errorf("watcher '%s': valor de max_wait '%s' inválido: %w", w.Name, w.MaxWait, err)
		}
		if maxWaitDur < debounceDur {
			return fmt.Errorf("watcher '%s': max_wait (%s) não pode ser menor que debounce (%s)", w.Name, w.MaxWait, w.Debounce)
		}
		w.MaxWaitDuration = maxWaitDur

		if len(w.Pipelines) == 0 {
			return fmt.Errorf("watcher '%s': deve referenciar ao menos um pipeline", w.Name)
		}

		for _, pName := range w.Pipelines {
			if _, exists := pipelines[pName]; !exists {
				return fmt.Errorf("watcher '%s': referencia o pipeline inexistente '%s'", w.Name, pName)
			}
		}
	}

	return nil
}
