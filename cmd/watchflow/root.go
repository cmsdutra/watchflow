package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
)

var (
	cfgFile string
	verbose bool

	version = "0.1.0-dev"
	commit  = "none"
	date    = "unknown"

	socketFlag string

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
	rootCmd.PersistentFlags().StringVar(&socketFlag, "socket", "", "Caminho alternativo para o socket Unix do daemon")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Habilita saída de log detalhada em modo debug")

	rootCmd.AddCommand(versionCmd)
}

// Execute starts the WatchFlow command-line interface execution.
func Execute() error {
	return rootCmd.Execute()
}

func resolveSocketPath() string {
	if socketFlag != "" {
		return config.ExpandPath(socketFlag)
	}

	cfgPath := cfgFile
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}

	if cfg, err := config.Load(cfgPath); err == nil && cfg.Daemon.SocketPath != "" {
		return config.ExpandPath(cfg.Daemon.SocketPath)
	}

	uid := os.Getuid()
	runUser := fmt.Sprintf("/run/user/%d", uid)
	if fi, err := os.Stat(runUser); err == nil && fi.IsDir() {
		return filepath.Join(runUser, "watchflow.sock")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ".watchflow.sock"
	}
	return filepath.Join(home, ".local", "state", "watchflow", "watchflow.sock")
}
