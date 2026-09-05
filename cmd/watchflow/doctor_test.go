package main

import (
	"database/sql"
	"os"
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

func TestDoctor_WALSize(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.db")

	// Sem arquivo -WAL: estado esperado após encerramento gracioso.
	if res := checkWALSize(dbPath); res.Status != StatusOK {
		t.Errorf("esperava StatusOK sem WAL presente, obteve %s (%s)", res.Status, res.Message)
	}

	// WAL pequeno permanece OK.
	if err := os.WriteFile(dbPath+"-wal", make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	res := checkWALSize(dbPath)
	if res.Status != StatusOK {
		t.Errorf("esperava StatusOK para WAL pequeno, obteve %s (%s)", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "KiB") {
		t.Errorf("esperava tamanho legível na mensagem, obteve: %s", res.Message)
	}

	// Acima da marca d'água o check precisa avisar, e não falhar: um WAL grande
	// não impede o daemon de operar.
	if err := os.WriteFile(dbPath+"-wal", make([]byte, walHighWaterMark+1), 0600); err != nil {
		t.Fatal(err)
	}
	if res := checkWALSize(dbPath); res.Status != StatusWarn {
		t.Errorf("esperava StatusWarn para WAL acima da marca d'água, obteve %s", res.Status)
	}
}

func TestDoctor_HumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{4096, "4.0 KiB"},
		{16 << 20, "16.0 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, esperava %q", c.in, got, c.want)
		}
	}
}

func TestDoctor_ServiceUnit(t *testing.T) {
	// O resultado depende do systemd da máquina, então o teste fixa apenas o
	// contrato: categoria própria e status dentro do conjunto conhecido.
	res := checkServiceUnit()
	if res.Category != "Serviço" {
		t.Errorf("esperava categoria 'Serviço', obteve %q", res.Category)
	}
	switch res.Status {
	case StatusOK, StatusWarn, StatusFail, StatusInfo:
	default:
		t.Errorf("status inesperado: %q", res.Status)
	}
	if res.Message == "" {
		t.Error("esperava mensagem não vazia")
	}
}

func TestDoctor_EvaluateServiceUnit(t *testing.T) {
	cases := []struct {
		name       string
		props      map[string]string
		wantStatus CheckStatus
		wantInMsg  string
	}{
		{
			name:       "unidade não instalada orienta a instalar",
			props:      map[string]string{"LoadState": "not-found"},
			wantStatus: StatusInfo,
			wantInMsg:  "SIGHUP",
		},
		{
			// Propriedades observadas no loop real de 2026-09-05, em que a
			// unidade falhava em 218/CAPABILITIES antes de executar o binário.
			name: "loop de reinício é falha crítica",
			props: map[string]string{
				"LoadState": "loaded", "ActiveState": "activating",
				"SubState": "auto-restart", "Result": "exit-code",
				"NRestarts": "6725", "UnitFileState": "enabled",
			},
			wantStatus: StatusFail,
			wantInMsg:  "6725",
		},
		{
			name: "unidade failed é falha crítica",
			props: map[string]string{
				"LoadState": "loaded", "ActiveState": "failed",
				"SubState": "failed", "Result": "exit-code",
				"UnitFileState": "enabled",
			},
			wantStatus: StatusFail,
			wantInMsg:  "failed",
		},
		{
			name: "unidade ativa e estável",
			props: map[string]string{
				"LoadState": "loaded", "ActiveState": "active",
				"SubState": "running", "Result": "success",
				"NRestarts": "0", "UnitFileState": "enabled",
			},
			wantStatus: StatusOK,
			wantInMsg:  "auto-start habilitado",
		},
		{
			name: "ativa com reinícios acumulados vira aviso",
			props: map[string]string{
				"LoadState": "loaded", "ActiveState": "active",
				"SubState": "running", "Result": "success",
				"NRestarts": "3", "UnitFileState": "enabled",
			},
			wantStatus: StatusWarn,
			wantInMsg:  "3 reinícios",
		},
		{
			name: "unidade instalada mas parada é informativa",
			props: map[string]string{
				"LoadState": "loaded", "ActiveState": "inactive",
				"SubState": "dead", "Result": "success",
				"NRestarts": "0", "UnitFileState": "disabled",
			},
			wantStatus: StatusInfo,
			wantInMsg:  "sem auto-start",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := evaluateServiceUnit(c.props)
			if res.Status != c.wantStatus {
				t.Errorf("status = %s, esperava %s (msg: %s)", res.Status, c.wantStatus, res.Message)
			}
			if !strings.Contains(res.Message, c.wantInMsg) {
				t.Errorf("mensagem %q não contém %q", res.Message, c.wantInMsg)
			}
			if c.wantStatus == StatusFail && res.Remediation == "" {
				t.Error("falha crítica deve trazer remediação acionável")
			}
		})
	}
}

func TestDoctor_MergeDriversInAttributes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "driver personalizado é extraído",
			content: ".obsidian/workspace.json merge=theirs\n",
			want:    []string{"theirs"},
		},
		{
			name:    "drivers embutidos são ignorados",
			content: "*.png merge=binary\n*.txt merge=text\nCHANGELOG merge=union\n",
			want:    nil,
		},
		{
			name:    "comentários e linhas vazias não confundem",
			content: "# comentário merge=fantasma\n\n*.json merge=theirs\n",
			want:    []string{"theirs"},
		},
		{
			name:    "formas de desativação não nomeiam driver",
			content: "*.bin -merge\n*.lock !merge\n",
			want:    nil,
		},
		{
			name:    "nomes repetidos aparecem uma vez, em ordem",
			content: "a.json merge=theirs\nb.json merge=ours\nc.json merge=theirs\n",
			want:    []string{"ours", "theirs"},
		},
		{
			name:    "padrão que contém 'merge=' não vira driver",
			content: "merge=isso-e-um-caminho.txt merge=theirs\n",
			want:    []string{"theirs"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeDriversInAttributes(c.content)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, esperava %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got %v, esperava %v", got, c.want)
				}
			}
		})
	}
}

func TestDoctor_CheckMergeDrivers(t *testing.T) {
	// Reproduz a condição do incidente: .gitattributes versionado chega no
	// clone, a definição local do driver não.
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile(".gitattributes", ".obsidian/workspace.json merge=theirs\n")
	sb.CommitAll("init")

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "cofre", Path: sb.RootDir, ResolvedPath: sb.RootDir},
		},
	}

	results := checkMergeDrivers(cfg)
	if len(results) != 1 {
		t.Fatalf("esperava 1 resultado, obteve %d", len(results))
	}
	if results[0].Status != StatusWarn {
		t.Errorf("esperava StatusWarn com driver ausente, obteve %s (%s)", results[0].Status, results[0].Message)
	}
	if !strings.Contains(results[0].Message, "theirs") {
		t.Errorf("mensagem deveria nomear o driver: %s", results[0].Message)
	}
	if results[0].Remediation == "" {
		t.Error("esperava remediação acionável")
	}

	// Definido o driver, o mesmo repositório passa a estar OK.
	sb.MustRunGit("config", "--local", "merge.theirs.driver", "cp %B %A")
	results = checkMergeDrivers(cfg)
	if len(results) != 1 || results[0].Status != StatusOK {
		t.Fatalf("esperava StatusOK após definir o driver, obteve %+v", results)
	}
}

func TestDoctor_CheckMergeDriversSemAtributos(t *testing.T) {
	// Repositório sem .gitattributes não deve gerar linha alguma no relatório.
	sb := testutil.NewGitSandbox(t)
	sb.WriteFile("nota.md", "conteúdo")
	sb.CommitAll("init")

	cfg := &config.Config{
		Watchers: []config.WatcherConfig{
			{Name: "cofre", Path: sb.RootDir, ResolvedPath: sb.RootDir},
		},
	}

	if results := checkMergeDrivers(cfg); len(results) != 0 {
		t.Errorf("esperava nenhum resultado sem .gitattributes, obteve %+v", results)
	}
}
