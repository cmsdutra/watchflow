package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/queue"
)

// TestJobsAndRunsExposeQueueState valida o caminho completo do coordenador:
// fila SQLite -> DTO do IPC, que é a base sobre a qual a TUI será construída.
func TestJobsAndRunsExposeQueueState(t *testing.T) {
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = coord.Shutdown(ctx)
	})

	store := coord.Store()
	if err := store.EnqueueJob(&queue.Job{
		ID: "job-a", WatcherID: "vault", PipelineName: "noop",
		PayloadFiles: []string{"n1.md", "n2.md"}, MaxRetries: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueJob(&queue.Job{
		ID: "job-b", WatcherID: "vault", PipelineName: "noop", MaxRetries: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobBlocked("job-b", "conflito de merge detectado"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPipelineRun(&queue.PipelineRun{
		ID: "run-a", JobID: "job-a", WatcherID: "vault", PipelineName: "noop",
		Status: "FAILED", DurationMs: 450, ErrorStep: "git.push", ErrorDetails: "sem rede",
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	jobsRes, err := coord.Jobs(ctx, "", 0)
	if err != nil {
		t.Fatalf("Jobs falhou: %v", err)
	}
	if len(jobsRes.Jobs) != 2 {
		t.Fatalf("esperava 2 jobs, obteve %d", len(jobsRes.Jobs))
	}

	byID := map[string]int{}
	for i, j := range jobsRes.Jobs {
		byID[j.ID] = i
	}

	pending := jobsRes.Jobs[byID["job-a"]]
	if pending.Files != 2 {
		t.Errorf("esperava 2 arquivos no job pendente, obteve %d", pending.Files)
	}
	if pending.MaxRetries != 4 {
		t.Errorf("max_retries não propagado: %d", pending.MaxRetries)
	}
	if pending.ScheduledFor == "" {
		t.Error("job PENDING deveria expor o agendamento")
	}

	blocked := jobsRes.Jobs[byID["job-b"]]
	if blocked.Status != string(queue.StatusBlocked) {
		t.Errorf("status = %s", blocked.Status)
	}
	if blocked.LastError == "" {
		t.Error("motivo do bloqueio não foi exposto")
	}
	if blocked.ScheduledFor != "" {
		t.Error("job bloqueado não deveria exibir agendamento — ele não vai rodar")
	}

	runsRes, err := coord.Runs(ctx, "", 0)
	if err != nil {
		t.Fatalf("Runs falhou: %v", err)
	}
	if len(runsRes.Runs) != 1 {
		t.Fatalf("esperava 1 execução, obteve %d", len(runsRes.Runs))
	}
	run := runsRes.Runs[0]
	if run.ErrorStep != "git.push" || run.DurationMs != 450 {
		t.Errorf("dados da execução não propagados: %+v", run)
	}
	if run.CreatedAt == "" {
		t.Error("timestamp da execução não foi formatado")
	}
}

func TestJobsRejectsUnknownWatcher(t *testing.T) {
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = coord.Shutdown(ctx)
	})

	if _, err := coord.Jobs(context.Background(), "inexistente", 0); err == nil {
		t.Error("esperava erro para watcher inexistente em Jobs")
	}
	if _, err := coord.Runs(context.Background(), "inexistente", 0); err == nil {
		t.Error("esperava erro para watcher inexistente em Runs")
	}
}

// TestTimestampsAreShownInLocalTime cobre a regressão em que o SQLite grava
// CURRENT_TIMESTAMP em UTC e a exibição não convertia: em UTC-3 uma
// sincronização das 23h aparecia como 02h do dia seguinte.
func TestTimestampsAreShownInLocalTime(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo") // UTC+9, offset grande e sem DST
	if err != nil {
		t.Skip("fuso indisponível no sistema")
	}
	original := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = original })

	cfg, _ := newTestConfig(t, "noop", []config.Step{})
	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = coord.Shutdown(ctx)
	})

	if err := coord.Store().UpdateWatcherEventTime("vault"); err != nil {
		t.Fatal(err)
	}

	res, err := coord.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Watchers) == 0 || res.Watchers[0].LastEventAt == "" {
		t.Fatal("esperava last_event_at preenchido")
	}

	shown, err := time.ParseInLocation("2006-01-02 15:04:05", res.Watchers[0].LastEventAt, loc)
	if err != nil {
		t.Fatalf("formato inesperado: %v", err)
	}

	// O horário exibido deve corresponder ao instante real, não ao UTC cru
	if drift := time.Since(shown); drift < -time.Minute || drift > time.Minute {
		t.Errorf("horário exibido (%s) diverge do instante real em %s; provável falta de conversão de fuso",
			res.Watchers[0].LastEventAt, drift.Truncate(time.Second))
	}
}
