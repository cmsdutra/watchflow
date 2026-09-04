package git_test

import (
	"context"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	gitprovider "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/tests/testutil"
)

func TestPush_NoRemote_Skipped(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("local.md", "# Local")
	sb.CommitAll("commit local")

	action := &gitprovider.PushAction{}
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "no-remote-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if !res.Skipped {
		t.Errorf("esperava res.Skipped == true quando não há remote")
	}
}

func TestPush_Success(t *testing.T) {
	central, machA, machB := setupTwoMachinesWithCentral(t)
	_ = central

	action := &gitprovider.PushAction{}

	// Cria alteração na Machine A
	machA.WriteFile("artigo.md", "# Artigo do WatchFlow")
	machA.CommitAll("adiciona artigo.md")

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao executar git.push: %v", err)
	}
	if !res.Success {
		t.Errorf("esperava res.Success == true")
	}

	// Machine B faz fetch e verifica se o commit foi recebido no central
	machB.MustRunGit("fetch", "origin", "main")
	machB.MustRunGit("merge", "--ff-only", "origin/main")
	if machB.ReadFile("artigo.md") != "# Artigo do WatchFlow" {
		t.Fatalf("commit não foi entregue ao remote central após push")
	}

	// Segundo push imediato (Everything up-to-date)
	res2, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha no segundo push: %v", err)
	}
	if !res2.Success {
		t.Errorf("esperava sucesso no push com tudo atualizado")
	}
}

func TestPush_TransientNetworkFailure(t *testing.T) {
	_, machA, _ := setupTwoMachinesWithCentral(t)
	action := &gitprovider.PushAction{}

	// Altera URL do remote origin para um host inalcançável
	machA.MustRunGit("remote", "set-url", "origin", "http://127.0.0.1:59999/repo.git")

	machA.WriteFile("offline_note.md", "offline")
	machA.CommitAll("commit offline")

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err == nil {
		t.Fatalf("esperava erro de rede ao tentar dar push para remote offline")
	}

	if res.Success {
		t.Errorf("esperava res.Success == false")
	}
	if !res.TransientErr {
		t.Errorf("esperava res.TransientErr == true para falha de conexão")
	}
}

func TestPush_PipelineRetryFlowWithOfflineRemote(t *testing.T) {
	central, machA, _ := setupTwoMachinesWithCentral(t)

	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&queue.WatcherRecord{
		ID:     "machA-vault",
		Name:   "machA-vault",
		Path:   machA.RootDir,
		Status: queue.WatcherHealthy,
	})

	reg := providers.NewRegistry()
	_ = gitprovider.Register(reg)

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "machA-vault", Path: machA.RootDir, ResolvedPath: machA.RootDir},
		},
		Pipelines: map[string]config.Pipeline{
			"sync_remote": {
				Timeout: "5s",
				Steps: []config.Step{
					{Action: "git.check_locks"},
					{Action: "git.add"},
					{Action: "git.commit"},
					{Action: "git.safe_sync"},
					{Action: "git.push"},
				},
			},
		},
	}

	runner := pipeline.NewRunner(store, reg, cfg)

	// Simula rede offline apontando remote para porta fechada
	machA.MustRunGit("remote", "set-url", "origin", "http://127.0.0.1:59999/repo.git")

	machA.WriteFile("nota_offline.md", "criado offline")

	job := &queue.Job{
		ID:           "job-offline-push",
		WatcherID:    "machA-vault",
		PipelineName: "sync_remote",
		PayloadFiles: []string{"nota_offline.md"},
		RetryCount:   0,
		MaxRetries:   3,
	}
	_ = store.EnqueueJob(job)

	res, err := runner.ExecuteJob(context.Background(), job)
	if err == nil {
		t.Fatalf("esperava falha no pipeline por remote offline")
	}

	// 1. Erro classificado como transitório
	if res.Category != pipeline.CategoryTransient {
		t.Errorf("esperava CategoryTransient, obteve %v", res.Category)
	}

	// 2. Watcher no SQLite deve estar DEGRADED
	w, _ := store.GetWatcherByName("machA-vault")
	if w.Status != queue.WatcherDegraded {
		t.Errorf("esperava status DEGRADED no watcher, obteve %s", w.Status)
	}

	// 3. Restaura a conexão com o remote central
	machA.MustRunGit("remote", "set-url", "origin", central)

	// 4. Nova tentativa após conexão restabelecida
	jobRetry := &queue.Job{
		ID:           "job-offline-push",
		WatcherID:    "machA-vault",
		PipelineName: "sync_remote",
		PayloadFiles: []string{},
		RetryCount:   1,
		MaxRetries:   3,
	}

	resRetry, errRetry := runner.ExecuteJob(context.Background(), jobRetry)
	if errRetry != nil {
		t.Fatalf("esperava sucesso após reconexão: %v", errRetry)
	}
	if !resRetry.Success {
		t.Errorf("esperava resRetry.Success == true")
	}

	// 5. Watcher volta para HEALTHY
	wRecovered, _ := store.GetWatcherByName("machA-vault")
	if wRecovered.Status != queue.WatcherHealthy {
		t.Errorf("esperava status HEALTHY após reconexão bem-sucedida, obteve %s", wRecovered.Status)
	}
}

func TestPush_NonFastForwardError(t *testing.T) {
	_, machA, machB := setupTwoMachinesWithCentral(t)
	action := &gitprovider.PushAction{}

	// Ambas máquinas criam commits concorrentes
	machB.WriteFile("b.md", "b")
	machB.CommitAll("b")
	machB.MustRunGit("push", "origin", "main")

	machA.WriteFile("a.md", "a")
	machA.CommitAll("a")

	// Machine A tenta dar push sem safe_sync antes (vai falhar por non-fast-forward)
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err == nil {
		t.Fatalf("esperava erro de push rejeitado (non-fast-forward)")
	}

	// Push rejeitado deve ser classificado como erro transitório (para reprocessamento com safe_sync)
	if !res.TransientErr {
		t.Errorf("esperava res.TransientErr == true para push rejeitado")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "rejected") && !strings.Contains(strings.ToLower(err.Error()), "fetch first") {
		t.Logf("mensagem de rejeição: %v", err)
	}
}
