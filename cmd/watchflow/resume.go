package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var resumeCmd = &cobra.Command{
	Use:   "resume [nome-do-watcher]",
	Short: "Retoma a captura de eventos de um watcher pausado ou interrompido",
	Long:  `Descongela o monitoramento do watcher especificado (ou de todos os watchers caso omitido) e reabilita o enfileiramento de pipelines.`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runResume,
}

func init() {
	rootCmd.AddCommand(resumeCmd)
}

func runResume(cmd *cobra.Command, args []string) error {
	sockPath := resolveSocketPath()
	client := ipc.NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	watcherName := ""
	if len(args) > 0 {
		watcherName = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Resume(ctx, watcherName)
	if err != nil {
		return fmt.Errorf("falha ao retomar watcher: %w", err)
	}

	fmt.Printf("▶ %s\n", resp.Message)
	return nil
}
