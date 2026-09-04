package queue

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar SQLite store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 1. Verifica modo WAL
	var journalMode string
	err = store.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode)
	if err != nil {
		t.Fatalf("falha ao verificar journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("esperava journal_mode 'wal', obteve '%s'", journalMode)
	}

	// 2. Registra Watcher
	w := &WatcherRecord{
		ID:     "w-1",
		Name:   "personal-vault",
		Path:   "/home/user/vault",
		Status: WatcherHealthy,
	}
	if err := store.RegisterWatcher(w); err != nil {
		t.Fatalf("falha ao registrar watcher: %v", err)
	}

	// 3. Busca por ID e Nome
	got, err := store.GetWatcher("w-1")
	if err != nil || got == nil {
		t.Fatalf("falha ao buscar watcher por ID: %v", err)
	}
	if got.Name != "personal-vault" {
		t.Errorf("esperava nome 'personal-vault', obteve: %s", got.Name)
	}

	byName, err := store.GetWatcherByName("personal-vault")
	if err != nil || byName == nil {
		t.Fatalf("falha ao buscar watcher por Nome: %v", err)
	}
	if byName.ID != "w-1" {
		t.Errorf("esperava ID 'w-1', obteve: %s", byName.ID)
	}

	// 4. Atualiza Status e Sync Time
	if err := store.UpdateWatcherStatus("w-1", WatcherDegraded, "wifi offline"); err != nil {
		t.Fatalf("falha ao atualizar status: %v", err)
	}
	updated, _ := store.GetWatcher("w-1")
	if updated.Status != WatcherDegraded || updated.LastErrorMessage != "wifi offline" {
		t.Errorf("status não atualizado corretamente: %v", updated)
	}

	if err := store.UpdateWatcherSyncTime("w-1", true, ""); err != nil {
		t.Fatalf("falha ao registrar sync bem-sucedido: %v", err)
	}
	synced, _ := store.GetWatcher("w-1")
	if synced.Status != WatcherHealthy || synced.LastSuccessSyncAt == nil {
		t.Errorf("status após sync com sucesso incorreto: %v", synced)
	}

	// 5. Lista watchers
	list, err := store.ListWatchers()
	if err != nil || len(list) != 1 {
		t.Errorf("esperava 1 watcher na listagem, obteve: %d", len(list))
	}
}

func TestJobEnqueueDequeueAtomic(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&WatcherRecord{ID: "w-1", Name: "v", Path: "/p", Status: WatcherHealthy})

	// 1. Enfileira Job
	job := &Job{
		ID:           "job-1",
		WatcherID:    "w-1",
		PipelineName: "git-sync",
		PayloadFiles: []string{"nota1.md", "nota2.md"},
		ScheduledFor: time.Now().Add(-1 * time.Second), // Elegível imediatamente
	}
	if err := store.EnqueueJob(job); err != nil {
		t.Fatalf("falha ao enfileirar job: %v", err)
	}

	// 2. Dequeue atômico
	dequeued, err := store.DequeueNextPending("worker-A")
	if err != nil || dequeued == nil {
		t.Fatalf("esperava receber job no dequeue: %v", err)
	}
	if dequeued.ID != "job-1" {
		t.Errorf("esperava job-1, obteve %s", dequeued.ID)
	}
	if dequeued.Status != StatusRunning {
		t.Errorf("status deveria ser RUNNING, obteve %s", dequeued.Status)
	}
	if dequeued.LockedBy != "worker-A" {
		t.Errorf("esperava worker-A, obteve %s", dequeued.LockedBy)
	}
	if len(dequeued.PayloadFiles) != 2 {
		t.Errorf("esperava 2 arquivos no payload, obteve %d", len(dequeued.PayloadFiles))
	}

	// 3. Tentativa de segundo dequeue simultâneo deve retornar nil (exclusão mútua)
	second, err := store.DequeueNextPending("worker-B")
	if err != nil {
		t.Fatalf("segundo dequeue falhou com erro: %v", err)
	}
	if second != nil {
		t.Errorf("segundo dequeue deveria ser nil, pois o único job está RUNNING")
	}

	// 4. Marca como concluído
	if err := store.MarkJobCompleted("job-1"); err != nil {
		t.Fatalf("falha ao marcar job como concluído: %v", err)
	}
}

func TestJobRetryAndExhaustion(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&WatcherRecord{ID: "w-1", Name: "v", Path: "/p", Status: WatcherHealthy})

	job := &Job{
		ID:           "retry-job",
		WatcherID:    "w-1",
		PipelineName: "pipe",
		PayloadFiles: []string{"a.md"},
		MaxRetries:   2,
		ScheduledFor: time.Now().Add(-1 * time.Second),
	}
	if err := store.EnqueueJob(job); err != nil {
		t.Fatalf("falha ao enfileirar: %v", err)
	}

	// Dequeue tentativa 1
	j1, _ := store.DequeueNextPending("worker-1")
	if j1 == nil {
		t.Fatal("esperava job-1")
	}

	// Falha tentativa 1 com backoff curto (50ms)
	err = store.MarkJobFailed("retry-job", "erro de rede", true, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("falha no MarkJobFailed: %v", err)
	}

	// Enquanto o backoff não expirar, dequeue retorna nil
	early, _ := store.DequeueNextPending("worker-1")
	if early != nil {
		t.Errorf("não deveria desenfileirar antes do scheduled_for")
	}

	// Aguarda expiração do backoff
	time.Sleep(60 * time.Millisecond)

	// Dequeue tentativa 2
	j2, err := store.DequeueNextPending("worker-1")
	if err != nil || j2 == nil {
		t.Fatalf("esperava dequeue da tentativa 2: %v", err)
	}
	if j2.RetryCount != 1 {
		t.Errorf("esperava RetryCount 1, obteve %d", j2.RetryCount)
	}

	// Falha tentativa 2 (esgota max_retries = 2)
	err = store.MarkJobFailed("retry-job", "erro fatal rede", true, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("falha no MarkJobFailed final: %v", err)
	}

	// Verifica se virou FAILED definitivo
	var status JobStatus
	_ = store.db.QueryRow("SELECT status FROM jobs WHERE id = ?", "retry-job").Scan(&status)
	if status != StatusFailed {
		t.Errorf("esperava status FAILED após esgotamento, obteve %s", status)
	}
}

func TestJobBlockedOnConflict(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&WatcherRecord{ID: "w-1", Name: "v", Path: "/p", Status: WatcherHealthy})
	_ = store.EnqueueJob(&Job{
		ID:           "conflict-job",
		WatcherID:    "w-1",
		PipelineName: "pipe",
		PayloadFiles: []string{"nota.md"},
	})

	_ = store.MarkJobBlocked("conflict-job", "merge conflict on nota.md")

	var status JobStatus
	var lastErr string
	_ = store.db.QueryRow("SELECT status, last_error FROM jobs WHERE id = ?", "conflict-job").Scan(&status, &lastErr)

	if status != StatusBlocked {
		t.Errorf("esperava status BLOCKED, obteve %s", status)
	}
	if lastErr != "merge conflict on nota.md" {
		t.Errorf("motivo do bloqueio incorreto: %s", lastErr)
	}
}

func TestCrashRecoveryResetRunningJobs(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "crash.db")

	// 1. Inicializa, enfileira e marca como RUNNING
	store1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	_ = store1.RegisterWatcher(&WatcherRecord{ID: "w-1", Name: "v", Path: "/p", Status: WatcherHealthy})
	_ = store1.EnqueueJob(&Job{
		ID:           "crash-job-1",
		WatcherID:    "w-1",
		PipelineName: "pipe",
		PayloadFiles: []string{"a.md"},
	})
	_ = store1.EnqueueJob(&Job{
		ID:           "crash-job-2",
		WatcherID:    "w-1",
		PipelineName: "pipe",
		PayloadFiles: []string{"b.md"},
	})

	_, _ = store1.DequeueNextPending("worker-1")
	_, _ = store1.DequeueNextPending("worker-2")

	// Simula crash abrupto: encerra conexão
	_ = store1.Close()

	// 2. Reabre o banco após o crash
	store2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao reabrir banco pós-crash: %v", err)
	}
	defer func() { _ = store2.Close() }()

	// Executa recuperação de jobs órfãos
	recoveredCount, err := store2.ResetRunningJobs()
	if err != nil {
		t.Fatalf("falha ao resetar jobs em RUNNING: %v", err)
	}
	if recoveredCount != 2 {
		t.Errorf("esperava recuperar 2 jobs órfãos, obteve: %d", recoveredCount)
	}

	// Os 2 jobs devem estar novamente disponíveis como PENDING
	pending, err := store2.ListPendingJobs("w-1")
	if err != nil || len(pending) != 2 {
		t.Errorf("esperava 2 jobs PENDING após recuperação, obteve: %d", len(pending))
	}
}

func TestPipelineRunAudit(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "audit.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&WatcherRecord{ID: "w-1", Name: "v", Path: "/p", Status: WatcherHealthy})
	_ = store.EnqueueJob(&Job{
		ID:           "job-10",
		WatcherID:    "w-1",
		PipelineName: "git-sync",
		PayloadFiles: []string{"nota.md"},
	})

	run := &PipelineRun{
		ID:           "run-1",
		JobID:        "job-10",
		WatcherID:    "w-1",
		PipelineName: "git-sync",
		Status:       "SUCCESS",
		DurationMs:   340,
	}

	if err := store.RecordPipelineRun(run); err != nil {
		t.Fatalf("falha ao registrar execução de pipeline: %v", err)
	}
}

func TestInMemoryStoreAndSyncFailure(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao criar banco in-memory: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&WatcherRecord{ID: "w-fail", Name: "v-fail", Path: "/p", Status: WatcherHealthy})

	if err := store.UpdateWatcherSyncTime("w-fail", false, "connection refused"); err != nil {
		t.Fatalf("falha ao registrar falha de sync: %v", err)
	}

	w, err := store.GetWatcher("w-fail")
	if err != nil || w == nil {
		t.Fatalf("falha ao consultar watcher: %v", err)
	}
	if w.LastErrorMessage != "connection refused" || w.LastFailedSyncAt == nil {
		t.Errorf("dados de falha de sync incorretos: %v", w)
	}
}

func TestGetWatcherNonExistent(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("falha ao criar store: %v", err)
	}
	defer func() { _ = store.Close() }()

	w, err := store.GetWatcher("nao_existe")
	if err != nil {
		t.Errorf("não deveria retornar erro para ID inexistente, obteve: %v", err)
	}
	if w != nil {
		t.Errorf("esperava nil para watcher inexistente")
	}

	wName, err := store.GetWatcherByName("nao_existe")
	if err != nil {
		t.Errorf("não deveria retornar erro para nome inexistente, obteve: %v", err)
	}
	if wName != nil {
		t.Errorf("esperava nil para watcher inexistente por nome")
	}
}
