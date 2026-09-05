package queue_test

import (
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/queue"
)

func seedJobs(t *testing.T, store *queue.Store) {
	t.Helper()

	for _, id := range []string{"j1", "j2", "j3"} {
		if err := store.EnqueueJob(&queue.Job{
			ID: id, WatcherID: "w", PipelineName: "p",
			PayloadFiles: []string{"a.md", "b.md"}, MaxRetries: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestListRecentJobsIncludesAllStatuses distingue esta consulta de
// ListPendingJobs: a inspeção da fila precisa mostrar também o que já terminou
// ou ficou bloqueado, que é justamente o que o usuário vai investigar.
func TestListRecentJobsIncludesAllStatuses(t *testing.T) {
	store := newStoreWithWatcher(t)
	seedJobs(t, store)

	if err := store.MarkJobCompleted("j1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobBlocked("j2", "conflito de merge detectado"); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.ListRecentJobs("", 0)
	if err != nil {
		t.Fatalf("falha ao listar: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("esperava 3 jobs de todos os status, obteve %d", len(jobs))
	}

	byID := map[string]*queue.Job{}
	for _, j := range jobs {
		byID[j.ID] = j
	}

	if byID["j2"].Status != queue.StatusBlocked {
		t.Errorf("j2 deveria estar BLOCKED, está %s", byID["j2"].Status)
	}
	// last_error precisa vir preenchido: é o motivo da investigação
	if byID["j2"].LastError == "" {
		t.Error("last_error do job bloqueado não foi carregado")
	}
	if len(byID["j3"].PayloadFiles) != 2 {
		t.Errorf("payload_files não foi deserializado: %v", byID["j3"].PayloadFiles)
	}
}

func TestListRecentJobsRespectsLimitAndWatcher(t *testing.T) {
	store := newStoreWithWatcher(t)
	seedJobs(t, store)

	jobs, err := store.ListRecentJobs("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Errorf("limite não respeitado: %d", len(jobs))
	}

	// Watcher inexistente não devolve nada; watcher vazio devolve tudo
	jobs, err = store.ListRecentJobs("outro", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("esperava lista vazia para outro watcher, obteve %d", len(jobs))
	}

	jobs, err = store.ListRecentJobs("w", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Errorf("esperava 3 jobs do watcher 'w', obteve %d", len(jobs))
	}
}

func TestListRecentRuns(t *testing.T) {
	store := newStoreWithWatcher(t)
	seedJobs(t, store)

	if err := store.RecordPipelineRun(&queue.PipelineRun{
		ID: "r1", JobID: "j1", WatcherID: "w", PipelineName: "p",
		Status: "SUCCESS", DurationMs: 1200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPipelineRun(&queue.PipelineRun{
		ID: "r2", WatcherID: "w", PipelineName: "p", Status: "FAILED",
		DurationMs: 90, ErrorStep: "git.push", ErrorDetails: "host inacessível",
	}); err != nil {
		t.Fatal(err)
	}

	runs, err := store.ListRecentRuns("w", 0)
	if err != nil {
		t.Fatalf("falha ao listar execuções: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("esperava 2 execuções, obteve %d", len(runs))
	}

	var failed *queue.PipelineRun
	for _, r := range runs {
		if r.Status == "FAILED" {
			failed = r
		}
	}
	if failed == nil {
		t.Fatal("execução falha não retornada")
	}
	if failed.ErrorStep != "git.push" || failed.ErrorDetails == "" {
		t.Errorf("detalhes da falha não foram carregados: %+v", failed)
	}
	// job_id NULL não pode quebrar a leitura
	if failed.JobID != "" {
		t.Errorf("job_id nulo deveria virar string vazia, obteve %q", failed.JobID)
	}
}

func TestListRecentDefaultsLimit(t *testing.T) {
	store := newStoreWithWatcher(t)

	for i := 0; i < queue.DefaultListLimit+10; i++ {
		if err := store.EnqueueJob(&queue.Job{
			ID:        time.Now().Format("150405.000000000") + string(rune('a'+i%26)) + string(rune(i)),
			WatcherID: "w", PipelineName: "p", MaxRetries: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	jobs, err := store.ListRecentJobs("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != queue.DefaultListLimit {
		t.Errorf("esperava o limite padrão de %d, obteve %d", queue.DefaultListLimit, len(jobs))
	}
}
