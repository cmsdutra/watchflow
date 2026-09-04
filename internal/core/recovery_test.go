package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/tests/testutil"
)

func TestStartupRecovery_ResetRunningJobs(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	store, err := queue.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("falha ao inicializar SQLite: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&queue.WatcherRecord{
		ID:     "v1",
		Name:   "vault-1",
		Path:   tempDir,
		Status: queue.WatcherHealthy,
	})

	// Job 1: RUNNING com retry_count 0 -> Deve ir para PENDING com retry_count 1
	_ = store.EnqueueJob(&queue.Job{
		ID:           "orphan-job-1",
		WatcherID:    "v1",
		PipelineName: "sync",
		PayloadFiles: []string{"a.md"},
		RetryCount:   0,
		MaxRetries:   5,
	})
	// Job 2: RUNNING com retry_count 4 e max_retries 5 -> Deve ir para FAILED
	_ = store.EnqueueJob(&queue.Job{
		ID:           "crash-loop-job",
		WatcherID:    "v1",
		PipelineName: "sync",
		PayloadFiles: []string{"b.md"},
		RetryCount:   4,
		MaxRetries:   5,
	})

	// Aloca ambos para RUNNING
	_, _ = store.DequeueNextPending("worker-1")
	_, _ = store.DequeueNextPending("worker-2")

	report, err := core.RunStartupRecovery(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("falha na recuperação de startup: %v", err)
	}

	if report.JobsReset != 2 {
		t.Errorf("esperava 2 jobs resetados, obteve %d", report.JobsReset)
	}

	// Verifica orphan-job-1
	pending, err := store.ListPendingJobs("v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "orphan-job-1" {
		t.Fatalf("esperava apenas orphan-job-1 em PENDING")
	}
	if pending[0].RetryCount != 1 {
		t.Errorf("esperava retry_count = 1, obteve %d", pending[0].RetryCount)
	}

	// Verifica crash-loop-job (deve ter virado FAILED definitivo)
	pendingAll, err := store.ListPendingJobs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range pendingAll {
		if j.ID == "crash-loop-job" {
			t.Errorf("job com tentativas esgotadas não deveria estar em PENDING: %+v", j)
		}
	}
}

func TestStartupRecovery_CleansDanglingMerge(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("init.txt", "base")
	sb.CommitAll("initial")

	// Cria branch divergente com conflito
	sb.MustRunGit("checkout", "-b", "feature")
	sb.WriteFile("shared.md", "branch feature")
	sb.CommitAll("feature commit")

	sb.MustRunGit("checkout", "main")
	sb.WriteFile("shared.md", "branch main")
	sb.CommitAll("main commit")

	// Tenta merge para forçar conflito e deixar MERGE_HEAD ativo
	_, _ = sb.RunGit("merge", "feature")

	mergeHead := filepath.Join(sb.RootDir, ".git", "MERGE_HEAD")
	if _, err := os.Stat(mergeHead); os.IsNotExist(err) {
		t.Fatalf("esperava MERGE_HEAD ativo para simular queda de energia")
	}

	watchers := []config.WatcherConfig{
		{Name: "test-vault", Path: sb.RootDir, ResolvedPath: sb.RootDir},
	}

	report, err := core.RunStartupRecovery(context.Background(), nil, watchers)
	if err != nil {
		t.Fatalf("falha ao rodar recuperação: %v", err)
	}

	if len(report.ReposCleaned) != 1 {
		t.Errorf("esperava 1 repositório limpo, obteve %d", len(report.ReposCleaned))
	}

	// MERGE_HEAD deve ter sido removido por git merge --abort
	if _, err := os.Stat(mergeHead); !os.IsNotExist(err) {
		t.Errorf("MERGE_HEAD ainda existe após recuperação")
	}

	// Árvore de trabalho limpa
	status := sb.MustRunGit("status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("worktree não ficou limpo após recuperação: %s", status)
	}
}

func TestStartupRecovery_PreservesExternalIndexLock(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	lockFile := sb.CreateIndexLock()

	watchers := []config.WatcherConfig{
		{Name: "locked-vault", Path: sb.RootDir, ResolvedPath: sb.RootDir},
	}

	report, err := core.RunStartupRecovery(context.Background(), nil, watchers)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	// Deve conter aviso sobre index.lock
	foundWarn := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "index.lock") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("esperava aviso sobre index.lock nos warnings")
	}

	// REGRA ANTI-DATA-LOSS: index.lock de terceiros JAMAIS é deletado
	if _, err := os.Stat(lockFile); os.IsNotExist(err) {
		t.Errorf("VIOLAÇÃO ANTI-DATA-LOSS: index.lock foi deletado indevidamente no startup")
	}
}
