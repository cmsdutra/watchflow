package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Solicita o encerramento gracioso (shutdown) do daemon",
	Long:  `Envia um comando IPC ao processo daemon solicitando que conclua os jobs em execução e finalize com segurança.`,
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	sockPath := resolveSocketPath()
	client := ipc.NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Stop(ctx)
	if err != nil {
		return fmt.Errorf("falha ao solicitar parada do daemon: %w", err)
	}

	fmt.Printf("⏹ %s\n", resp.Message)
	return nil
}
