package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	cfgFile string
	verbose bool

	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"

	rootCmd = &cobra.Command{
		Use:   "watchflow",
		Short: "WatchFlow — Filesystem-driven autonomous automation daemon",
		Long: `WatchFlow é um daemon local reativo orientado a eventos de sistema de arquivos,
projetado para sincronização autônoma, bidirecional e estritamente segura de
vaults de conhecimento e diretórios locais via Git e outros provedores.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Exibe a versão do WatchFlow",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("WatchFlow v%s (commit: %s, date: %s)\n", version, commit, date)
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "Caminho para o arquivo de configuração (padrão ~/.config/watchflow/config.yaml)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Habilita saída de log detalhada em modo debug")

	rootCmd.AddCommand(versionCmd)
}

// Execute starts the WatchFlow command-line interface execution.
func Execute() error {
	return rootCmd.Execute()
}
