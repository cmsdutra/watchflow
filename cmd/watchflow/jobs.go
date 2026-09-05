package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/ipc"
)

var (
	jobsJSON    bool
	jobsLimit   int
	jobsWatcher string

	runsJSON    bool
	runsLimit   int
	runsWatcher string
)

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Lista o estado da fila persistente de jobs",
	Long: `Lista os jobs da fila SQLite do daemon, do mais recente ao mais antigo,
incluindo os que aguardam retry e os bloqueados por conflito de merge.`,
	Example: `  watchflow jobs
  watchflow jobs --watcher meu-vault -n 20
  watchflow jobs --json | jq '.jobs[] | select(.status=="BLOCKED")'`,
	RunE: runJobs,
}

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Lista o histórico de execuções de pipeline",
	Long:  `Exibe a auditoria das execuções de pipeline registradas pelo daemon.`,
	Example: `  watchflow runs
  watchflow runs --watcher meu-vault -n 20
  watchflow runs --json | jq '.runs[] | select(.status!="SUCCESS")'`,
	RunE: runRuns,
}

func init() {
	jobsCmd.Flags().BoolVar(&jobsJSON, "json", false, "Exibe a resposta em formato JSON bruto")
	jobsCmd.Flags().IntVarP(&jobsLimit, "limit", "n", 20, "Quantidade máxima de jobs listados")
	jobsCmd.Flags().StringVar(&jobsWatcher, "watcher", "", "Filtra por nome de watcher")

	runsCmd.Flags().BoolVar(&runsJSON, "json", false, "Exibe a resposta em formato JSON bruto")
	runsCmd.Flags().IntVarP(&runsLimit, "limit", "n", 20, "Quantidade máxima de execuções listadas")
	runsCmd.Flags().StringVar(&runsWatcher, "watcher", "", "Filtra por nome de watcher")

	rootCmd.AddCommand(jobsCmd)
	rootCmd.AddCommand(runsCmd)
}

func runJobs(cmd *cobra.Command, _ []string) error {
	client := ipc.NewClient(resolveSocketPath())

	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()

	res, err := client.Jobs(ctx, jobsWatcher, jobsLimit)
	if err != nil {
		return fmt.Errorf("falha ao consultar a fila de jobs: %w", err)
	}

	out := cmd.OutOrStdout()

	if jobsJSON {
		return writeJSON(out, res)
	}

	if len(res.Jobs) == 0 {
		_, _ = fmt.Fprintln(out, "Nenhum job na fila.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tWATCHER\tPIPELINE\tSTATUS\tARQS\tTENTATIVAS\tDETALHE")
	_, _ = fmt.Fprintln(w, "--\t-------\t--------\t------\t----\t----------\t-------")

	for _, j := range res.Jobs {
		detail := j.LastError
		if detail == "" && j.ScheduledFor != "" {
			detail = "agendado para " + j.ScheduledFor
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d/%d\t%s\n",
			shortID(j.ID), j.WatcherID, j.PipelineName, j.Status,
			j.Files, j.RetryCount, j.MaxRetries, truncateCell(detail, 60))
	}

	return w.Flush()
}

func runRuns(cmd *cobra.Command, _ []string) error {
	client := ipc.NewClient(resolveSocketPath())

	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()

	res, err := client.Runs(ctx, runsWatcher, runsLimit)
	if err != nil {
		return fmt.Errorf("falha ao consultar o histórico de execuções: %w", err)
	}

	out := cmd.OutOrStdout()

	if runsJSON {
		return writeJSON(out, res)
	}

	if len(res.Runs) == 0 {
		_, _ = fmt.Fprintln(out, "Nenhuma execução registrada.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "QUANDO\tWATCHER\tPIPELINE\tSTATUS\tDURAÇÃO\tDETALHE")
	_, _ = fmt.Fprintln(w, "------\t-------\t--------\t------\t-------\t-------")

	for _, r := range res.Runs {
		detail := r.ErrorDetails
		if r.ErrorStep != "" {
			detail = r.ErrorStep + ": " + detail
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.CreatedAt, r.WatcherID, r.PipelineName, r.Status,
			(time.Duration(r.DurationMs) * time.Millisecond).String(),
			truncateCell(detail, 60))
	}

	return w.Flush()
}

func writeJSON(out interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shortID encurta os identificadores gerados com timestamp em nanossegundos,
// que ocupariam a largura inteira do terminal sem acrescentar informação.
func shortID(id string) string {
	const maxLen = 26
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen-1] + "…"
}

func truncateCell(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
