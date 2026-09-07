package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/ipc"
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

		// Avisos não invalidam a configuração, mas quase sempre indicam algo que
		// o usuário não pretendia.
		warnings := config.RepoWarnings(cfg)
		if len(warnings) > 0 {
			cmd.Println()
			for _, w := range warnings {
				cmd.Printf("  ⚠ %s\n", w)
			}
		}

		return nil
	},
}

var (
	addWatcherDebounce     string
	addWatcherMaxWait      string
	addWatcherPullInterval string
	addWatcherPipelines    []string
	addWatcherIgnore       []string
	addWatcherDisabled     bool
	skipReload             bool
)

var configAddWatcherCmd = &cobra.Command{
	Use:   "add-watcher <nome> <caminho>",
	Short: "Adiciona uma pasta vigiada ao config.yaml",
	Long: `Insere um novo watcher na seção 'watchers' do arquivo de configuração.

A edição preserva todos os comentários existentes no arquivo, incluindo os
dos demais watchers e pipelines — não é um dump da struct de configuração
inteira. O reencode do YAML pode, porém, remover linhas em branco entre
seções e reajustar o espaçamento de comentários à direita (# fica colado ao
valor em vez de alinhado em coluna).

O caminho informado precisa existir como diretório; a configuração resultante
é validada antes de ser gravada, então nada é escrito se o resultado for
inválido. Se nenhum --pipeline for informado, o comando tenta inferir um:
usa 'default' se ele já existir, o único pipeline declarado se houver apenas
um, ou pede para você escolher explicitamente quando for ambíguo.

Por padrão o comando tenta recarregar o daemon em seguida (equivalente a
'watchflow reload'); use --no-reload para só editar o arquivo.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		targetFile := cfgFile
		if targetFile == "" {
			targetFile = config.DefaultConfigPath()
		}

		name, path := args[0], args[1]
		ignore := addWatcherIgnore
		if len(ignore) == 0 {
			ignore = config.DefaultIgnorePatterns
		}

		wc := config.WatcherConfig{
			Name:         name,
			Path:         path,
			Debounce:     addWatcherDebounce,
			MaxWait:      addWatcherMaxWait,
			PullInterval: addWatcherPullInterval,
			Pipelines:    addWatcherPipelines,
			Ignore:       ignore,
		}
		if addWatcherDisabled {
			disabled := false
			wc.Enabled = &disabled
		}

		if err := config.AddWatcherToFile(targetFile, wc); err != nil {
			return fmt.Errorf("falha ao adicionar watcher: %w", err)
		}

		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "✓ Watcher '%s' adicionado a %s\n", name, config.ExpandPath(targetFile))

		if !skipReload {
			reloadAfterConfigEdit(cmd)
		}
		return nil
	},
}

var configRemoveWatcherCmd = &cobra.Command{
	Use:   "remove-watcher <nome>",
	Short: "Remove uma pasta vigiada do config.yaml",
	Long: `Exclui o watcher de nome dado da seção 'watchers' do arquivo de configuração.

Apenas remove o registro de monitoramento: os arquivos do diretório e o
repositório Git em si não são tocados. A edição preserva os comentários do
restante do arquivo (o reencode do YAML pode remover linhas em branco entre
seções), e a configuração resultante é validada antes de ser gravada.

Por padrão o comando tenta recarregar o daemon em seguida (equivalente a
'watchflow reload'); use --no-reload para só editar o arquivo.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		targetFile := cfgFile
		if targetFile == "" {
			targetFile = config.DefaultConfigPath()
		}

		name := args[0]
		if err := config.RemoveWatcherFromFile(targetFile, name); err != nil {
			return fmt.Errorf("falha ao remover watcher: %w", err)
		}

		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "✓ Watcher '%s' removido de %s\n", name, config.ExpandPath(targetFile))

		if !skipReload {
			reloadAfterConfigEdit(cmd)
		}
		return nil
	},
}

// reloadAfterConfigEdit tenta recarregar o daemon em execução após uma edição
// bem-sucedida de add-watcher/remove-watcher. É best-effort: se o daemon não
// estiver ativo, apenas orienta o usuário a rodar 'watchflow reload' depois.
func reloadAfterConfigEdit(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	client := ipc.NewClient(resolveSocketPath())

	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()

	res, err := client.Reload(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  (daemon não recarregado automaticamente: %v)\n  Execute 'watchflow reload' quando o daemon estiver ativo.\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "↻ %s\n", res.Message)
}

func init() {
	configAddWatcherCmd.Flags().StringVar(&addWatcherDebounce, "debounce", "", "Janela de debounce (padrão: 15s)")
	configAddWatcherCmd.Flags().StringVar(&addWatcherMaxWait, "max-wait", "", "Teto máximo de espera (padrão: 60s)")
	configAddWatcherCmd.Flags().StringVar(&addWatcherPullInterval, "pull-interval", "", "Cadência de consulta ao remoto (padrão: "+config.DefaultPullInterval+")")
	configAddWatcherCmd.Flags().StringSliceVar(&addWatcherPipelines, "pipeline", nil, "Pipeline a disparar (repetível; padrão: pipeline embutido)")
	configAddWatcherCmd.Flags().StringSliceVar(&addWatcherIgnore, "ignore", nil, "Padrão glob a ignorar (repetível; padrão: exclusões usuais de vault)")
	configAddWatcherCmd.Flags().BoolVar(&addWatcherDisabled, "disabled", false, "Cadastra o watcher desativado (enabled: false)")
	configAddWatcherCmd.Flags().BoolVar(&skipReload, "no-reload", false, "Não tenta recarregar o daemon após editar o arquivo")

	configRemoveWatcherCmd.Flags().BoolVar(&skipReload, "no-reload", false, "Não tenta recarregar o daemon após editar o arquivo")

	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configAddWatcherCmd)
	configCmd.AddCommand(configRemoveWatcherCmd)
	rootCmd.AddCommand(configCmd)
}
