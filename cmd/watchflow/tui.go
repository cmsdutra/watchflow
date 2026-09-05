package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/tui"
)

var tuiRefresh time.Duration

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Abre o painel interativo de monitoramento no terminal",
	Long: `Abre um painel interativo que acompanha em tempo real os cofres vigiados,
a fila persistente e os eventos recentes do daemon.

A interface é apenas um cliente do socket de controle: fechá-la não interrompe a
sincronização, que continua rodando em segundo plano.

Atalhos:
  s          sincroniza o watcher selecionado
  p          pausa ou retoma o watcher selecionado
  r          força uma atualização imediata
  Tab        alterna o painel em foco
  ↑ / ↓      navega entre os watchers
  q          sai da interface`,
	RunE: runTUI,
}

func init() {
	tuiCmd.Flags().DurationVar(&tuiRefresh, "refresh", time.Second,
		"Intervalo de atualização automática dos painéis")

	rootCmd.AddCommand(tuiCmd)
}

func runTUI(cmd *cobra.Command, _ []string) error {
	// Sem terminal interativo a interface não tem como desenhar; falhar cedo com
	// uma mensagem clara é melhor do que emitir códigos de escape num pipe.
	if !isTerminal(os.Stdout) {
		return fmt.Errorf("'watchflow tui' exige um terminal interativo; para uso em scripts prefira 'watchflow status --json'")
	}

	client := ipc.NewClient(resolveSocketPath())

	return tui.Run(tui.Options{
		Client:       client,
		Events:       tui.LogTail{Path: resolveLogPathQuietly()},
		Version:      version,
		RefreshEvery: tuiRefresh,
	})
}

// resolveLogPathQuietly descobre o log da configuração, tolerando ausência: a
// TUI funciona sem o painel de auditoria, mas não sem o socket.
func resolveLogPathQuietly() string {
	cfgPath := cfgFile
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return ""
	}
	return filepath.Join(config.ExpandPath(cfg.Daemon.StateDir), logger.LogFileName)
}
