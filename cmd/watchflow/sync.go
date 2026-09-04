package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var syncCmd = &cobra.Command{
	Use:   "sync [nome-do-watcher]",
	Short: "Força a execução imediata dos pipelines de sincronização",
	Long:  `Solicita ao daemon que ignore o timer de debounce e dispare imediatamente a sincronização para o repositório informado (ou para todos caso omitido).`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runSync,
}

func init() {
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	sockPath := resolveSocketPath()
	client := ipc.NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	watcherName := ""
	if len(args) > 0 {
		watcherName = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Sync(ctx, watcherName)
	if err != nil {
		return fmt.Errorf("falha ao solicitar sincronização: %w", err)
	}

	fmt.Printf("✓ %s\n", resp.Message)
	if len(resp.EnqueuedJobs) > 0 {
		fmt.Printf("  Jobs enfileirados: %v\n", resp.EnqueuedJobs)
	}
	return nil
}
