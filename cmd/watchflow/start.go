package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
)

var foregroundFlag bool

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Inicia o daemon do WatchFlow",
	Long:  `Inicia o daemon local de automação do WatchFlow, monitorando os diretórios configurados e processando pipelines.`,
	RunE:  runStart,
}

func init() {
	startCmd.Flags().BoolVarP(&foregroundFlag, "foreground", "f", false, "Executa o daemon no terminal foreground")
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) error {
	cfgPath := cfgFile
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("falha ao carregar configuração de '%s': %w", cfgPath, err)
	}

	if socketFlag != "" {
		cfg.Daemon.SocketPath = socketFlag
	}

	coordinator, err := core.NewCoordinator(cfg, version)
	if err != nil {
		return fmt.Errorf("falha ao inicializar coordenador do daemon: %w", err)
	}

	// Configura interceptação de sinais SIGTERM e SIGINT para graceful shutdown
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- coordinator.Start(ctx)
	}()

	fmt.Println("🚀 WatchFlow daemon iniciado com sucesso.")
	fmt.Printf("Versão: %s | PID: %d\n", version, os.Getpid())
	fmt.Printf("Socket IPC: %s\n", config.ExpandPath(cfg.Daemon.SocketPath))
	fmt.Printf("Watchers ativos: %d\n", len(cfg.Watchers))

	select {
	case <-ctx.Done():
		fmt.Println("\n🛑 Sinal de encerramento recebido (SIGINT/SIGTERM). Encerrando graciosamente...")
	case err := <-coordErrCh:
		if err != nil {
			return fmt.Errorf("erro durante execução do daemon: %w", err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := coordinator.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("erro durante shutdown: %w", err)
	}

	fmt.Println("✅ WatchFlow encerrado com sucesso.")
	return nil
}
