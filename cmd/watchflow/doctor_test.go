package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/tests/testutil"
	_ "modernc.org/sqlite"
)

func TestDoctor_GitRequirement(t *testing.T) {
	res := checkGitRequirement()
	if res.Status != StatusOK && res.Status != StatusWarn {
		t.Errorf("esperava Git válido no ambiente de teste, obteve status: %s (msg: %s)", res.Status, res.Message)
	}
}

func TestDoctor_StateDirectoryAndSQLiteIntegrity(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Diretório antes de criar banco
	results := checkStateDirectory(tempDir)
	if len(results) < 2 {
		t.Fatalf("esperava ao menos 2 verificações para state dir, obteve %d", len(results))
	}
	if results[0].Status != StatusOK {
		t.Errorf("esperava state directory OK, obteve: %s", results[0].Status)
	}

	// 2. Inicializa SQLite íntegro
	dbPath := filepath.Join(tempDir, "state.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	resultsWithDB := checkStateDirectory(tempDir)
	foundIntegrityOK := false
	for _, r := range resultsWithDB {
		if r.Name == "SQLite Integrity" && r.Status == StatusOK {
			foundIntegrityOK = true
			break
		}
	}
	if !foundIntegrityOK {
		t.Errorf("esperava integridade do SQLite com StatusOK")
	}
}

func TestDoctor_InotifyLimits(t *testing.T) {
	results := checkInotifyLimits()
	if len(results) == 0 {
		t.Fatalf("esperava ao menos um resultado para limites do inotify")
	}
}

func TestDoctor_CheckWatchers(t *testing.T) {
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("a.txt", "123")
	sb.CommitAll("init")

	nonExistentDir := filepath.Join(t.TempDir(), "ghost-dir")

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "valid-vault", Path: sb.RootDir, ResolvedPath: sb.RootDir},
			{Name: "missing-vault", Path: nonExistentDir, ResolvedPath: nonExistentDir},
		},
	}

	results := checkWatchers(cfg)
	if len(results) != 2 {
		t.Fatalf("esperava 2 resultados, obteve %d", len(results))
	}

	if results[0].Status != StatusOK {
		t.Errorf("esperava valid-vault com StatusOK, obteve %s", results[0].Status)
	}
	if results[1].Status != StatusFail {
		t.Errorf("esperava missing-vault com StatusFail, obteve %s", results[1].Status)
	}
}

func TestDoctor_CommandExecution(t *testing.T) {
	output, _ := executeCommand("doctor")

	if !strings.Contains(output, "diagnóstico do ambiente WatchFlow") {
		t.Errorf("saída esperada do doctor não encontrada: %s", output)
	}
	if !strings.Contains(output, "Git Version") {
		t.Errorf("esperava Git Version no relatório: %s", output)
	}
}
