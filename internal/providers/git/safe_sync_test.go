package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/providers"
	gitprovider "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/tests/testutil"
)

func setupTwoMachinesWithCentral(t *testing.T) (string, *testutil.GitSandbox, *testutil.GitSandbox) {
	t.Helper()

	central := testutil.NewBareRepo(t)

	// Inicializa um repo temporário para criar o commit inicial no central
	initSb := testutil.NewGitSandbox(t)
	initSb.WriteFile("init.txt", "initial content")
	initSb.CommitAll("initial commit")
	initSb.MustRunGit("remote", "add", "origin", central)
	initSb.MustRunGit("push", "-u", "origin", "main")

	// Clona para Machine A e Machine B
	machA := testutil.CloneRepo(t, central)
	machB := testutil.CloneRepo(t, central)

	return central, machA, machB
}

func TestSafeSync_NoRemote_Skipped(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("doc.md", "# Test")
	sb.CommitAll("local only")

	action := &gitprovider.SafeSyncAction{}
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    sb.RootDir,
		WatcherName: "local-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if !res.Skipped {
		t.Errorf("esperava res.Skipped == true para repo sem remote")
	}
}

func TestSafeSync_InSyncAndLocalAhead(t *testing.T) {
	_, machA, _ := setupTwoMachinesWithCentral(t)
	action := &gitprovider.SafeSyncAction{}

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	// 1. Em sincronia
	res1, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao sincronizar em dia: %v", err)
	}
	if !res1.Success {
		t.Errorf("esperava res1.Success == true")
	}

	// 2. Local à frente
	machA.WriteFile("local_note.md", "conteúdo local")
	machA.CommitAll("commit local em A")

	res2, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao sincronizar com local à frente: %v", err)
	}
	if !res2.Success {
		t.Errorf("esperava res2.Success == true")
	}
	if !strings.Contains(res2.Output, "à frente") {
		t.Errorf("esperava mensagem informando que local está à frente: %s", res2.Output)
	}
}

func TestSafeSync_FastForward(t *testing.T) {
	_, machA, machB := setupTwoMachinesWithCentral(t)
	action := &gitprovider.SafeSyncAction{}

	// Machine B cria alteração e envia ao remote central
	machB.WriteFile("from_b.md", "conteúdo da máquina B")
	machB.CommitAll("commit de B")
	machB.MustRunGit("push", "origin", "main")

	// Machine A executa safe_sync (fast-forward)
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha no fast-forward: %v", err)
	}
	if !res.Success {
		t.Errorf("esperava sucesso no fast-forward")
	}

	// Verifica se o arquivo da Machine B agora existe na Machine A
	content := machA.ReadFile("from_b.md")
	if content != "conteúdo da máquina B" {
		t.Errorf("conteúdo inesperado após fast-forward: %s", content)
	}
}

func TestSafeSync_CleanThreeWayMerge(t *testing.T) {
	_, machA, machB := setupTwoMachinesWithCentral(t)
	action := &gitprovider.SafeSyncAction{}

	// Machine B adiciona arquivo_b.md e dá push
	machB.WriteFile("arquivo_b.md", "conteúdo B")
	machB.CommitAll("commit paralelo B")
	machB.MustRunGit("push", "origin", "main")

	// Machine A adiciona arquivo_a.md e commita localmente (sem push)
	machA.WriteFile("arquivo_a.md", "conteúdo A")
	machA.CommitAll("commit paralelo A")

	// Machine A executa safe_sync (merge limpo sem conflito de linhas)
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha no merge limpo: %v", err)
	}
	if !res.Success {
		t.Errorf("esperava sucesso no merge limpo")
	}

	// Ambos os arquivos devem existir na Machine A
	if machA.ReadFile("arquivo_a.md") != "conteúdo A" {
		t.Errorf("arquivo_a.md ausente ou corrompido")
	}
	if machA.ReadFile("arquivo_b.md") != "conteúdo B" {
		t.Errorf("arquivo_b.md não integrado no merge")
	}
}

func TestSafeSync_MergeConflict_ImmediateAbortAndCleanWorktree(t *testing.T) {
	_, machA, machB := setupTwoMachinesWithCentral(t)
	action := &gitprovider.SafeSyncAction{}

	// Ambas as máquinas alteram o mesmo arquivo na mesma linha de forma conflitante
	machB.WriteFile("shared.md", "Versão da Máquina B")
	machB.CommitAll("conflito vindo de B")
	machB.MustRunGit("push", "origin", "main")

	machA.WriteFile("shared.md", "Versão da Máquina A")
	machA.CommitAll("conflito local em A")

	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err == nil {
		t.Fatalf("esperava erro de conflito em safe_sync, obteve nil")
	}

	if !errors.Is(err, providers.ErrConflict) {
		t.Errorf("esperava erro providers.ErrConflict, obteve: %v", err)
	}
	if !res.ConflictErr {
		t.Errorf("esperava res.ConflictErr == true")
	}

	// CRITÉRIO DE ACEITAÇÃO CRÍTICO ANTI-DATA-LOSS:
	// 1. O repositório nunca fica com arquivos contendo marcadores de conflito (<<<<<<< HEAD)
	sharedContent := machA.ReadFile("shared.md")
	if strings.Contains(sharedContent, "<<<<<<<") || strings.Contains(sharedContent, "=======") || strings.Contains(sharedContent, ">>>>>>>") {
		t.Fatalf("VIOLAÇÃO GRAVE ANTI-DATA-LOSS: marcadores de conflito foram deixados no arquivo: %s", sharedContent)
	}

	// 2. O conteúdo local original da Máquina A permanece intacto
	if sharedContent != "Versão da Máquina A" {
		t.Errorf("conteúdo da Máquina A foi alterado: %s", sharedContent)
	}

	// 3. A árvore de trabalho deve estar completamente limpa (git status vazio, sem estados 'UU')
	statusOut := machA.MustRunGit("status", "--porcelain")
	if strings.TrimSpace(statusOut) != "" {
		t.Fatalf("worktree não ficou limpo após git merge --abort: %s", statusOut)
	}
}

func TestSafeSync_DetachedHeadProtection(t *testing.T) {
	_, machA, _ := setupTwoMachinesWithCentral(t)
	headHash := machA.HeadHash()

	// Coloca o repositório em detached HEAD
	machA.MustRunGit("checkout", headHash)

	action := &gitprovider.SafeSyncAction{}
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	_, err := action.Execute(ctx)
	if err == nil {
		t.Fatalf("esperava recusa de execução em detached HEAD")
	}
	if !strings.Contains(err.Error(), "detached HEAD") {
		t.Errorf("mensagem de erro inesperada: %v", err)
	}
}
