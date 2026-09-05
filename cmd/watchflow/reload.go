package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var reloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Relê o arquivo de configuração sem reiniciar o daemon",
	Long: `Faz o daemon reler ~/.config/watchflow/config.yaml e aplicar as mudanças.

Watchers são adicionados, removidos e reiniciados individualmente; pipelines e
notificações passam a valer para os próximos jobs. Se a configuração estiver
inválida, nada é alterado e o daemon continua com a anterior.

Mudanças na seção 'daemon' (socket, diretório de estado, concorrência, nível de
log) exigem reiniciar o processo; o comando avisa quando for o caso.`,
	RunE: runReload,
}

func init() {
	rootCmd.AddCommand(reloadCmd)
}

func runReload(cmd *cobra.Command, _ []string) error {
	client := ipc.NewClient(resolveSocketPath())

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	res, err := client.Reload(ctx)
	if err != nil {
		return fmt.Errorf("falha ao recarregar a configuração: %w", err)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "↻ %s\n", res.Message)

	report := func(label string, names []string) {
		for _, n := range names {
			_, _ = fmt.Fprintf(out, "  %s %s\n", label, n)
		}
	}
	report("+", res.WatchersAdded)
	report("-", res.WatchersRemoved)
	report("~", res.WatchersUpdated)

	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(out, "\n  ⚠ %s\n", w)
	}

	if len(res.NeedsRestart) > 0 {
		_, _ = fmt.Fprintln(out, "\n  As mudanças abaixo só valem após reiniciar o daemon:")
		for _, item := range res.NeedsRestart {
			_, _ = fmt.Fprintf(out, "    · %s\n", item)
		}
		_, _ = fmt.Fprintln(out, "\n    systemctl --user restart watchflow")
	}

	if !res.Success {
		return fmt.Errorf("recarga concluída com falhas; verifique 'watchflow logs --level warn'")
	}

	return nil
}
