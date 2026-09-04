package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var (
	jsonOutput bool

	statusCmd = &cobra.Command{
		Use:   "status",
		Short: "Exibe o estado de integridade e métricas do daemon em tempo real",
		Long:  `Consulta o daemon via Unix socket e exibe uma tabela detalhada com o estado dos repositórios vigiados, métricas de sincronização e fila de jobs.`,
		RunE:  runStatus,
	}
)

func init() {
	statusCmd.Flags().BoolVar(&jsonOutput, "json", false, "Exibe a resposta em formato JSON bruto")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	sockPath := resolveSocketPath()
	client := ipc.NewClient(sockPath)
	client.SetTimeout(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("daemon inacessível: %w\nExecute 'watchflow start' para iniciar o serviço", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	renderStatusTable(status, sockPath)
	return nil
}

func renderStatusTable(s *ipc.StatusResponse, sockPath string) {
	fmt.Printf("\n=== WatchFlow Daemon ===\n")
	fmt.Printf("Status:       ATIVO (PID: %d)\n", s.DaemonPID)
	fmt.Printf("Versão:       %s\n", s.Version)
	fmt.Printf("Tempo Ativo:  %s\n", s.Uptime)
	fmt.Printf("Socket IPC:   %s\n", sockPath)
	fmt.Printf("Fila de Jobs: %d pendentes | %d executando | %d bloqueados\n\n", s.PendingJobs, s.RunningJobs, s.BlockedJobs)

	if len(s.Watchers) == 0 {
		fmt.Println("Nenhum diretório monitorado configurado.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPOSITÓRIO\tSTATUS\tÚLTIMO EVENTO\tÚLTIMO SUCESSO\tÚLTIMO ERRO")
	_, _ = fmt.Fprintln(w, "-----------\t------\t-------------\t--------------\t-----------")

	for _, wt := range s.Watchers {
		lastEvent := wt.LastEventAt
		if lastEvent == "" {
			lastEvent = "-"
		}
		lastSuccess := wt.LastSuccessSyncAt
		if lastSuccess == "" {
			lastSuccess = "-"
		}
		lastErr := wt.LastError
		if lastErr == "" {
			lastErr = "-"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", wt.Name, wt.Status, lastEvent, lastSuccess, lastErr)
	}

	_ = w.Flush()
	fmt.Println()
}
