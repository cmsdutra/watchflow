package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/logger"
)

var foregroundFlag bool

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Inicia o daemon do WatchFlow",
	Long:  `Inicia o daemon local de automação do WatchFlow, monitorando os diretórios configurados e processando pipelines.`,
	RunE:  runStart,
}

func init() {
	startCmd.Flags().BoolVarP(&foregroundFlag, "foreground", "f", false,
		"Força o resumo legível em stdout (por padrão exibido apenas quando stdout é um terminal)")
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

	// Logging estruturado precisa existir antes do coordenador para capturar
	// falhas de registro de watcher e o relatório de recuperação pós-crash.
	logLevel := cfg.Daemon.LogLevel
	if verbose {
		logLevel = "debug"
	}

	stateDir := config.ExpandPath(cfg.Daemon.StateDir)
	logCloser, err := logger.Setup(logger.Options{
		Level:  logLevel,
		Dir:    stateDir,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("falha ao inicializar o subsistema de logs: %w", err)
	}
	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	log := logger.For("daemon")
	log.Info("iniciando WatchFlow",
		slog.String("versao", version),
		slog.String("commit", commit),
		slog.String("config", cfgPath),
		slog.String("state_dir", stateDir),
		slog.String("log_level", logLevel))

	coordinator, err := core.NewCoordinator(cfg, version)
	if err != nil {
		return fmt.Errorf("falha ao inicializar coordenador do daemon: %w", err)
	}
	// Necessário para que 'watchflow reload' saiba qual arquivo reler
	coordinator.SetConfigPath(cfgPath)

	// Configura interceptação de sinais SIGTERM e SIGINT para graceful shutdown
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	coordErrCh := make(chan error, 1)
	go func() {
		coordErrCh <- coordinator.Start(ctx)
	}()

	// Sob systemd o resumo decorado é ruído no journald, que já recebe o log
	// estruturado; em um terminal ele é a confirmação que o usuário espera.
	if foregroundFlag || isTerminal(os.Stdout) {
		fmt.Println("🚀 WatchFlow daemon iniciado com sucesso.")
		fmt.Printf("Versão: %s | PID: %d\n", version, os.Getpid())
		fmt.Printf("Socket IPC: %s\n", config.ExpandPath(cfg.Daemon.SocketPath))
		fmt.Printf("Watchers ativos: %d\n", len(cfg.Watchers))
		fmt.Printf("Log JSON: %s\n", filepath.Join(stateDir, logger.LogFileName))
	}

	select {
	case <-ctx.Done():
		if foregroundFlag || isTerminal(os.Stdout) {
			fmt.Println("\n🛑 Sinal de encerramento recebido (SIGINT/SIGTERM). Encerrando graciosamente...")
		}
		log.Info("sinal de encerramento recebido")
	case err := <-coordErrCh:
		if err != nil {
			log.Error("daemon encerrou com erro", slog.Any("error", err))
			return fmt.Errorf("erro durante execução do daemon: %w", err)
		}
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := coordinator.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("erro durante shutdown: %w", err)
	}

	if foregroundFlag || isTerminal(os.Stdout) {
		fmt.Println("✅ WatchFlow encerrado com sucesso.")
	}
	return nil
}

// isTerminal informa se o descritor está ligado a um terminal interativo.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
