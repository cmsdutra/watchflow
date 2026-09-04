package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var pauseCmd = &cobra.Command{
	Use:   "pause [nome-do-watcher]",
	Short: "Pausa a captura de eventos e execução de pipelines",
	Long:  `Congela o processamento de eventos do watcher especificado (ou de todos os watchers caso omitido).`,
	Args:  cobra.MaximumNArgs(1),
	RunE:  runPause,
}

func init() {
	rootCmd.AddCommand(pauseCmd)
}

func runPause(cmd *cobra.Command, args []string) error {
	sockPath := resolveSocketPath()
	client := ipc.NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	watcherName := ""
	if len(args) > 0 {
		watcherName = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Pause(ctx, watcherName)
	if err != nil {
		return fmt.Errorf("falha ao pausar watcher: %w", err)
	}

	fmt.Printf("⏸ %s\n", resp.Message)
	return nil
}
