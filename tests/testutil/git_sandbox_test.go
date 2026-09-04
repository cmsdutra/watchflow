package testutil_test

import (
	"testing"

	"github.com/watchflow/watchflow/tests/testutil"
)

func TestGitSandbox(t *testing.T) {
	sb := testutil.NewGitSandbox(t)

	// Criação de arquivo
	sb.WriteFile("notas/diario.md", "# Meu Diário")
	content := sb.ReadFile("notas/diario.md")
	if content != "# Meu Diário" {
		t.Errorf("conteúdo inesperado: %s", content)
	}

	// Commit inicial
	commitHash := sb.CommitAll("initial commit")
	if len(commitHash) == 0 {
		t.Errorf("esperava hash válido de commit")
	}

	if sb.CommitCount() != 1 {
		t.Errorf("esperava 1 commit, encontrou %d", sb.CommitCount())
	}

	// Teste de lock
	lockPath := sb.CreateIndexLock()
	if lockPath == "" {
		t.Errorf("caminho de lock vazio")
	}
	sb.RemoveIndexLock()
}
