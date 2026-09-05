package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	// Popula providers.DefaultRegistry para que 'config validate' consiga
	// verificar os nomes de action declarados no YAML.
	_ "github.com/watchflow/watchflow/internal/providers/git"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Gerenciamento e validação de configurações do WatchFlow",
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Valida a sintaxe e a consistência semântica de um arquivo de configuração",
	RunE: func(cmd *cobra.Command, args []string) error {
		targetFile := cfgFile
		if targetFile == "" {
			targetFile = config.DefaultConfigPath()
		}

		expandedPath := config.ExpandPath(targetFile)
		if _, err := os.Stat(expandedPath); os.IsNotExist(err) {
			return fmt.Errorf("arquivo de configuração não encontrado em '%s'", expandedPath)
		}

		cfg, err := config.Load(targetFile)
		if err != nil {
			return fmt.Errorf("falha ao validar configuração: %w", err)
		}

		cmd.Printf("✓ Configuração válida! (%s)\n", expandedPath)
		cmd.Printf("  - Modo de log: %s\n", cfg.Daemon.LogLevel)
		cmd.Printf("  - Watchers configurados: %d\n", len(cfg.Watchers))
		for _, w := range cfg.Watchers {
			status := "ativo"
			if !w.IsEnabled() {
				status = "desativado"
			}
			cmd.Printf("    • [%s] %s -> %s (debounce: %s, max_wait: %s)\n",
				status, w.Name, w.ResolvedPath, w.Debounce, w.MaxWait)
		}
		cmd.Printf("  - Notificações: %v (backend: %s)\n", cfg.Notifications.Enabled, cfg.Notifications.Backend)
		cmd.Printf("  - Pipelines declarados: %d\n", len(cfg.Pipelines))
		for pName, p := range cfg.Pipelines {
			cmd.Printf("    • %s (timeout: %s, steps: %d)\n", pName, p.Timeout, len(p.Steps))
		}

		return nil
	},
}

func init() {
	configCmd.AddCommand(configValidateCmd)
	rootCmd.AddCommand(configCmd)
}
