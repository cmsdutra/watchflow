package git_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
	gitprovider "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/internal/queue"
	"github.com/watchflow/watchflow/tests/testutil"
)

func TestLockChecker_NoLock(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	checker := &gitprovider.LockCheckerAction{}

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "test-vault",
	}

	res, err := checker.Execute(ctx)
	if err != nil {
		t.Fatalf("esperava sucesso sem lock, obteve erro: %v", err)
	}
	if !res.Success {
		t.Errorf("esperava res.Success == true")
	}
}

func TestLockChecker_WaitsAndSucceedsWhenLockReleased(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	checker := &gitprovider.LockCheckerAction{}

	sb.CreateIndexLock()

	// Goroutine que libera o lock após 100ms
	go func() {
		time.Sleep(100 * time.Millisecond)
		sb.RemoveIndexLock()
	}()

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "test-vault",
		StepParams: map[string]interface{}{
			"max_wait":      "2s",
			"poll_interval": "50ms",
		},
	}

	res, err := checker.Execute(ctx)
	if err != nil {
		t.Fatalf("esperava liberação do lock com sucesso, obteve: %v", err)
	}
	if !res.Success {
		t.Errorf("esperava res.Success == true")
	}
}

func TestLockChecker_TimeoutWhenLockPersists(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	checker := &gitprovider.LockCheckerAction{}

	lockFile := sb.CreateIndexLock()

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "test-vault",
		StepParams: map[string]interface{}{
			"max_wait":      "100ms",
			"poll_interval": "20ms",
		},
	}

	res, err := checker.Execute(ctx)
	if err == nil {
		t.Fatalf("esperava erro de timeout com lock persistente")
	}

	if res.Success {
		t.Errorf("esperava res.Success == false")
	}
	if !res.TransientErr {
		t.Errorf("esperava res.TransientErr == true para lock externo")
	}

	// REGRA ANTI-DATA-LOSS: O daemon JAMAIS deve remover o arquivo .git/index.lock de terceiros!
	if _, statErr := os.Stat(lockFile); os.IsNotExist(statErr) {
		t.Errorf("VIOLAÇÃO ANTI-DATA-LOSS: o arquivo .git/index.lock foi removido indevidamente pelo daemon!")
	}
}

func TestAddAndCommit_NoChanges_Skipped(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("init.txt", "base")
	sb.CommitAll("initial commit")

	addAction := &gitprovider.AddAction{}
	commitAction := &gitprovider.CommitAction{}

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "vault-clean",
	}

	// 1. Add sem alterações
	addRes, err := addAction.Execute(ctx)
	if err != nil {
		t.Fatalf("erro inesperado no add: %v", err)
	}
	if !addRes.Skipped {
		t.Errorf("esperava addRes.Skipped == true quando não há alterações")
	}

	// 2. Commit sem alterações
	initialCommitCount := sb.CommitCount()
	commitRes, err := commitAction.Execute(ctx)
	if err != nil {
		t.Fatalf("erro inesperado no commit: %v", err)
	}
	if !commitRes.Skipped {
		t.Errorf("esperava commitRes.Skipped == true quando não há alterações")
	}

	// Não deve ter criado nenhum commit extra
	if sb.CommitCount() != initialCommitCount {
		t.Errorf("VIOLAÇÃO: commit vazio foi criado! Esperava %d, encontrou %d", initialCommitCount, sb.CommitCount())
	}
}

func TestAddAndCommit_WithChanges_Success(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("init.txt", "base")
	sb.CommitAll("initial")

	// Modifica arquivo existente e cria um novo
	sb.WriteFile("init.txt", "base modificada")
	sb.WriteFile("novo.md", "# Novo Documento")

	addAction := &gitprovider.AddAction{}
	commitAction := &gitprovider.CommitAction{}

	ctx := &providers.StepContext{
		Context:      context.Background(),
		BasePath:     sb.RootDir,
		WatcherName:  "my-vault",
		ChangedFiles: []string{"init.txt", "novo.md"},
		StepParams: map[string]interface{}{
			"message": "sync: atualização de notas em {date} ({watcher})",
		},
	}

	// 1. Add
	addRes, err := addAction.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao executar add: %v", err)
	}
	if !addRes.Success || addRes.Skipped {
		t.Errorf("esperava sucesso e não skipped no add: %+v", addRes)
	}
	if len(addRes.AffectedPaths) != 2 {
		t.Errorf("esperava 2 caminhos afetados, obteve %d", len(addRes.AffectedPaths))
	}

	// 2. Commit
	initialCount := sb.CommitCount()
	commitRes, err := commitAction.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao executar commit: %v", err)
	}
	if !commitRes.Success || commitRes.Skipped {
		t.Errorf("esperava sucesso e não skipped no commit: %+v", commitRes)
	}

	if sb.CommitCount() != initialCount+1 {
		t.Errorf("esperava incremento de 1 commit")
	}

	// Validação da mensagem formatada no log do git
	logOut, _ := sb.RunGit("log", "-1", "--pretty=%B")
	if !strings.Contains(logOut, "my-vault") {
		t.Errorf("mensagem de commit não substituiu {watcher}: %s", logOut)
	}
}

func TestFullPipelineWithGitProvider(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("readme.md", "# Readme Inicial")
	sb.CommitAll("init")

	store, err := queue.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	_ = store.RegisterWatcher(&queue.WatcherRecord{
		ID:     "git-vault",
		Name:   "git-vault",
		Path:   sb.RootDir,
		Status: queue.WatcherHealthy,
	})

	reg := providers.NewRegistry()
	if err := gitprovider.Register(reg); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "git-vault", Path: sb.RootDir, ResolvedPath: sb.RootDir},
		},
		Pipelines: map[string]config.Pipeline{
			"local_sync": {
				Timeout: "10s",
				Steps: []config.Step{
					{Action: "git.check_locks"},
					{Action: "git.add"},
					{Action: "git.commit", Params: map[string]interface{}{
						"message": "watchflow: commit automático {timestamp}",
					}},
				},
			},
		},
	}

	runner := pipeline.NewRunner(store, reg, cfg)

	// Caso 1: Arquivo alterado -> Commit criado
	sb.WriteFile("anotacoes/reuniao.md", "Pauta 1")
	job1 := &queue.Job{
		ID:           "job-git-1",
		WatcherID:    "git-vault",
		PipelineName: "local_sync",
		PayloadFiles: []string{"anotacoes/reuniao.md"},
	}
	_ = store.EnqueueJob(job1)

	res1, err := runner.ExecuteJob(context.Background(), job1)
	if err != nil {
		t.Fatalf("esperava sucesso na execução do pipeline git: %v", err)
	}
	if !res1.Success {
		t.Errorf("res1.Success esperado true")
	}

	if sb.CommitCount() != 2 {
		t.Errorf("esperava 2 commits após execução, obteve %d", sb.CommitCount())
	}

	// Caso 2: Nenhuma alteração nova -> Pipeline executado com sucesso e commit skipped
	job2 := &queue.Job{
		ID:           "job-git-2",
		WatcherID:    "git-vault",
		PipelineName: "local_sync",
		PayloadFiles: []string{},
	}
	_ = store.EnqueueJob(job2)

	res2, err := runner.ExecuteJob(context.Background(), job2)
	if err != nil {
		t.Fatalf("esperava sucesso no pipeline sem alterações: %v", err)
	}
	if !res2.Success {
		t.Errorf("res2.Success esperado true")
	}

	// Contagem de commits deve permanecer inalterada (2 commits)
	if sb.CommitCount() != 2 {
		t.Errorf("não deveria criar commit adicional quando stage está limpo, contagem: %d", sb.CommitCount())
	}
}

func TestSanitizeGitCredentials(t *testing.T) {
	raw := "fatal: could not read Username for 'https://ghp_secr3tToken123456@github.com/org/repo.git': No such device"
	sanitized := gitprovider.SanitizeGitOutput(raw)
	if strings.Contains(sanitized, "ghp_secr3tToken123456") {
		t.Errorf("token não foi ocultado da saída: %s", sanitized)
	}
	if !strings.Contains(sanitized, "https://***@github.com/org/repo.git") {
		t.Errorf("esperava substituição com https://***@: %s", sanitized)
	}

	rawUserPass := "remote: error: https://user:superSecretPass@gitlab.com/vault.git"
	sanitizedUserPass := gitprovider.SanitizeGitOutput(rawUserPass)
	if strings.Contains(sanitizedUserPass, "superSecretPass") {
		t.Errorf("senha de usuário não foi mascarada: %s", sanitizedUserPass)
	}
}
