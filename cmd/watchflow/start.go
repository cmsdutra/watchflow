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
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
)

var (
	foregroundFlag bool
	detachFlag     bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Inicia o daemon do WatchFlow",
	Long:  `Inicia o daemon local de automação do WatchFlow, monitorando os diretórios configurados e processando pipelines.`,
	RunE:  runStart,
}

func init() {
	startCmd.Flags().BoolVarP(&foregroundFlag, "foreground", "f", false,
		"Força o resumo legível em stdout (por padrão exibido apenas quando stdout é um terminal)")
	startCmd.Flags().BoolVarP(&detachFlag, "detach", "d", false,
		"Inicia o daemon em segundo plano, desligado do terminal (somente Windows)")
	rootCmd.AddCommand(startCmd)
}

// detachChildArgs remonta a linha de comando do processo filho preservando as
// flags que importam para ele. O '--detach' é deliberadamente omitido: com ele,
// o filho relançaria outro filho, indefinidamente.
func detachChildArgs() []string {
	args := []string{"start"}
	if cfgFile != "" {
		args = append(args, "--config", cfgFile)
	}
	if socketFlag != "" {
		args = append(args, "--socket", socketFlag)
	}
	if verbose {
		args = append(args, "--verbose")
	}
	return args
}

func runStart(cmd *cobra.Command, args []string) error {
	if detachFlag {
		// Idempotência é o contrato de '--detach': quem o chama é um supervisor
		// (a tarefa do Agendador, que roda de tempos em tempos para levantar o
		// daemon caso ele tenha caído). Sem esta checagem, cada passagem com o
		// daemon de pé criaria um processo que morre na trava de socket e deixa
		// um ERROR no log — transformando a supervisão em ruído periódico.
		if ipc.NewClient(resolveSocketPath()).IsDaemonRunning() {
			fmt.Println("WatchFlow já está em execução; nada a fazer.")
			return nil
		}

		pid, err := spawnDetached(detachChildArgs())
		if err != nil {
			return err
		}
		fmt.Printf("🚀 WatchFlow iniciado em segundo plano (PID: %d).\n", pid)
		fmt.Println("Ele sobrevive ao fechamento deste terminal.")
		fmt.Println("Acompanhe com 'watchflow status' e encerre com 'watchflow stop'.")
		return nil
	}

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

	// Um Start que falhou também passa pelo Shutdown: workers e socket já
	// estão de pé, e sair sem fechá-los deixaria jobs em RUNNING.
	var runErr error
	select {
	case <-ctx.Done():
		if foregroundFlag || isTerminal(os.Stdout) {
			fmt.Println("\n🛑 Sinal de encerramento recebido (SIGINT/SIGTERM). Encerrando graciosamente...")
		}
		log.Info("sinal de encerramento recebido")
	case <-coordinator.StopRequested():
		// Não depende do retorno de Start: se o boot estiver travado, esperar
		// por ele deixaria o processo vivo sem socket.
	case err := <-coordErrCh:
		if err != nil {
			log.Error("daemon encerrou com erro", slog.Any("error", err))
			runErr = fmt.Errorf("erro durante execução do daemon: %w", err)
		}
	}

	// Um encerramento forçado não muda o código de saída: numa parada pedida
	// pelo usuário, um erro faria o systemd (Restart=on-failure) religar o daemon.
	if err := shutdownBounded(coordinator); err != nil {
		log.Error("encerramento forçado", slog.Any("error", err))
	}
	if runErr != nil {
		return runErr
	}

	if foregroundFlag || isTerminal(os.Stdout) {
		fmt.Println("✅ WatchFlow encerrado com sucesso.")
	}
	return nil
}

const (
	shutdownTimeout = 10 * time.Second
	// shutdownHardLimit cobre o prazo gracioso, a espera extra pelos workers
	// e o fechamento do IPC e do banco, com folga.
	shutdownHardLimit = 30 * time.Second
)

// shutdownBounded desiste de esperar um Shutdown pendurado. Sair assim é
// seguro para o repositório: os processos git são filhos sem vínculo de
// término com o daemon e concluem sozinhos, liberando o próprio index.lock, e
// jobs deixados em RUNNING são retomados pela recuperação do próximo boot.
func shutdownBounded(c *core.Coordinator) error {
	return runBounded(shutdownHardLimit, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return c.Shutdown(ctx)
	})
}

// runBounded executa fn e devolve erro se ela não terminar dentro de limit.
func runBounded(limit time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("encerramento não concluiu em %s; saindo sem esperar", limit)
	}
}

// isTerminal informa se o descritor está ligado a um terminal interativo.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
