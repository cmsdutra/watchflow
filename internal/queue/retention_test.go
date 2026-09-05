package queue_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/queue"
)

func newStoreWithWatcher(t *testing.T) *queue.Store {
	t.Helper()

	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao abrir SQLite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.RegisterWatcher(&queue.WatcherRecord{
		ID: "w", Name: "w", Path: "/tmp/w", Status: queue.WatcherHealthy,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestPurgePreservesUnfinishedWork garante que a retenção nunca descarta
// trabalho pendente: só jobs terminais e histórico de auditoria são removidos.
func TestPurgePreservesUnfinishedWork(t *testing.T) {
	store := newStoreWithWatcher(t)

	// Um job de cada estado relevante
	for _, id := range []string{"done", "failed", "pending", "blocked"} {
		if err := store.EnqueueJob(&queue.Job{ID: id, WatcherID: "w", PipelineName: "p", MaxRetries: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkJobCompleted("done"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobFailed("failed", "erro", false, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobBlocked("blocked", "conflito"); err != nil {
		t.Fatal(err)
	}

	// CURRENT_TIMESTAMP tem granularidade de segundo. Com uma retenção
	// sub-segundo o corte é o segundo corrente, e 1,2s de espera garante que os
	// registros caiam em um segundo estritamente anterior — sem depender da fase
	// sub-segundo em que o teste começou.
	time.Sleep(1200 * time.Millisecond)

	res, err := store.PurgeOldRecords(time.Millisecond)
	if err != nil {
		t.Fatalf("falha ao purgar: %v", err)
	}
	if res.Jobs != 2 {
		t.Errorf("esperava 2 jobs terminais removidos (COMPLETED + FAILED), obteve %d", res.Jobs)
	}

	counts, err := store.CountJobsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts[queue.StatusPending] != 1 {
		t.Errorf("job PENDING foi removido pela retenção: %v", counts)
	}
	if counts[queue.StatusBlocked] != 1 {
		t.Errorf("job BLOCKED (conflito aguardando o usuário) foi removido: %v", counts)
	}
	if counts[queue.StatusCompleted] != 0 || counts[queue.StatusFailed] != 0 {
		t.Errorf("jobs terminais antigos deveriam ter sido removidos: %v", counts)
	}
}

func TestPurgeKeepsRecentHistory(t *testing.T) {
	store := newStoreWithWatcher(t)

	if err := store.EnqueueJob(&queue.Job{ID: "recente", WatcherID: "w", PipelineName: "p", MaxRetries: 5}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobCompleted("recente"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPipelineRun(&queue.PipelineRun{
		ID: "run1", JobID: "recente", WatcherID: "w", PipelineName: "p", Status: "SUCCESS",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := store.PurgeOldRecords(queue.DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	if res.Jobs != 0 || res.Runs != 0 {
		t.Errorf("histórico dentro da janela de retenção não deveria ser removido: %+v", res)
	}
}

func TestCountJobsByStatus(t *testing.T) {
	store := newStoreWithWatcher(t)

	for _, id := range []string{"a", "b", "c"} {
		if err := store.EnqueueJob(&queue.Job{ID: id, WatcherID: "w", PipelineName: "p", MaxRetries: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DequeueNextPending("worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkJobBlocked("b", "conflito"); err != nil {
		t.Fatal(err)
	}

	counts, err := store.CountJobsByStatus()
	if err != nil {
		t.Fatal(err)
	}
	if counts[queue.StatusRunning] != 1 {
		t.Errorf("esperava 1 job RUNNING, obteve %d", counts[queue.StatusRunning])
	}
	if counts[queue.StatusBlocked] != 1 {
		t.Errorf("esperava 1 job BLOCKED, obteve %d", counts[queue.StatusBlocked])
	}
	if counts[queue.StatusPending] != 1 {
		t.Errorf("esperava 1 job PENDING, obteve %d", counts[queue.StatusPending])
	}
}

// TestConcurrentDequeueNeverDuplicates confirma que a transação IMMEDIATE
// serializa a alocação: um mesmo job jamais é entregue a dois workers.
func TestConcurrentDequeueNeverDuplicates(t *testing.T) {
	store := newStoreWithWatcher(t)

	const total = 40
	for i := 0; i < total; i++ {
		if err := store.EnqueueJob(&queue.Job{
			ID:        string(rune('a'+i%26)) + time.Now().Format("150405.000000000") + string(rune(i)),
			WatcherID: "w", PipelineName: "p", MaxRetries: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]bool)
	for {
		job, err := store.DequeueNextPending("worker")
		if err != nil {
			t.Fatalf("dequeue falhou: %v", err)
		}
		if job == nil {
			break
		}
		if seen[job.ID] {
			t.Fatalf("job '%s' foi alocado duas vezes", job.ID)
		}
		seen[job.ID] = true
	}

	if len(seen) != total {
		t.Errorf("esperava %d jobs alocados, obteve %d", total, len(seen))
	}
}

// TestPurgeIsTimezoneIndependent fixa a regressão em que o corte era calculado
// em Go (hora local) e comparado com colunas gravadas pelo SQLite em UTC. O
// deslocamento do fuso fazia a retenção não podar nada em offsets negativos e
// apagar histórico recente cedo demais em offsets positivos.
func TestPurgeIsTimezoneIndependent(t *testing.T) {
	for _, tz := range []string{"UTC", "America/Sao_Paulo", "Asia/Tokyo"} {
		t.Run(tz, func(t *testing.T) {
			loc, err := time.LoadLocation(tz)
			if err != nil {
				t.Skipf("fuso '%s' indisponível no sistema", tz)
			}
			original := time.Local
			time.Local = loc
			t.Cleanup(func() { time.Local = original })

			store := newStoreWithWatcher(t)
			if err := store.EnqueueJob(&queue.Job{ID: "j", WatcherID: "w", PipelineName: "p", MaxRetries: 5}); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkJobCompleted("j"); err != nil {
				t.Fatal(err)
			}

			// Ainda dentro da janela: não pode ser removido
			res, err := store.PurgeOldRecords(time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if res.Jobs != 0 {
				t.Errorf("job recente foi purgado indevidamente em %s: %d", tz, res.Jobs)
			}

			// Fora da janela: deve ser removido
			time.Sleep(1200 * time.Millisecond)
			res, err = store.PurgeOldRecords(time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if res.Jobs != 1 {
				t.Errorf("job antigo não foi purgado em %s: %d", tz, res.Jobs)
			}
		})
	}
}

// TestConcurrentWorkersDoNotHitDatabaseLocked cobre a regressão observada num
// daemon real com dois workers: as PRAGMAs eram aplicadas via db.Exec, que
// atinge só uma conexão do pool. As demais nasciam sem busy_timeout e a
// contenção virava "database is locked" imediato, derrubando o dequeue.
func TestConcurrentWorkersDoNotHitDatabaseLocked(t *testing.T) {
	dir := t.TempDir()
	store, err := queue.NewSQLiteStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("falha ao abrir banco: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.RegisterWatcher(&queue.WatcherRecord{
		ID: "w", Name: "w", Path: dir, Status: queue.WatcherHealthy,
	}); err != nil {
		t.Fatal(err)
	}

	const totalJobs = 60
	for i := 0; i < totalJobs; i++ {
		if err := store.EnqueueJob(&queue.Job{
			ID:        fmt.Sprintf("job-%03d", i),
			WatcherID: "w", PipelineName: "p", MaxRetries: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		claimed  = map[string]string{}
		failures []string
	)

	// Mais workers do que o pool, para forçar contenção real
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", worker)

			for {
				job, err := store.DequeueNextPending(id)
				if err != nil {
					mu.Lock()
					failures = append(failures, err.Error())
					mu.Unlock()
					return
				}
				if job == nil {
					return
				}

				mu.Lock()
				if prev, dup := claimed[job.ID]; dup {
					failures = append(failures, fmt.Sprintf("job %s alocado por %s e %s", job.ID, prev, id))
				}
				claimed[job.ID] = id
				mu.Unlock()

				if err := store.MarkJobCompleted(job.ID); err != nil {
					mu.Lock()
					failures = append(failures, err.Error())
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d falha(s) sob contenção, primeira: %s", len(failures), failures[0])
	}
	if len(claimed) != totalJobs {
		t.Errorf("esperava %d jobs processados, obteve %d", totalJobs, len(claimed))
	}
}
