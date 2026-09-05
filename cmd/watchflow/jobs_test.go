package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/ipc"
)

func TestCLI_JobsTable(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	out, err := executeCommand("jobs", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro ao executar jobs: %v (%s)", err, out)
	}

	for _, want := range []string{"WATCHER", "PENDING", "BLOCKED", "obsidian-vault", "vault-sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("esperava %q na tabela:\n%s", want, out)
		}
	}

	// Tentativas exibidas como n/max
	if !strings.Contains(out, "1/5") {
		t.Errorf("esperava contagem de tentativas '1/5':\n%s", out)
	}
	// O motivo do bloqueio é a informação que o usuário procura
	if !strings.Contains(out, "conflito de merge") {
		t.Errorf("esperava o motivo do bloqueio na coluna de detalhe:\n%s", out)
	}
	// Job pendente mostra o agendamento
	if !strings.Contains(out, "agendado para") {
		t.Errorf("esperava o agendamento do job pendente:\n%s", out)
	}
}

func TestCLI_JobsJSON(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	out, err := executeCommand("jobs", "--socket", sockPath, "--json")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	var res ipc.JobsResponse
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("saída não é JSON válido: %v\n%s", err, out)
	}
	if len(res.Jobs) != 2 {
		t.Fatalf("esperava 2 jobs, obteve %d", len(res.Jobs))
	}
	if res.Jobs[0].Files != 3 {
		t.Errorf("contagem de arquivos não preservada: %+v", res.Jobs[0])
	}
}

func TestCLI_RunsTable(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	out, err := executeCommand("runs", "--socket", sockPath)
	if err != nil {
		t.Fatalf("erro ao executar runs: %v (%s)", err, out)
	}

	if !strings.Contains(out, "SUCCESS") || !strings.Contains(out, "FAILED") {
		t.Errorf("esperava ambos os status:\n%s", out)
	}
	// Duração legível, não milissegundos crus
	if !strings.Contains(out, "1.25s") {
		t.Errorf("esperava duração formatada '1.25s':\n%s", out)
	}
	// O step que falhou precisa aparecer junto do detalhe
	if !strings.Contains(out, "git.push: could not resolve host") {
		t.Errorf("esperava step e detalhe do erro:\n%s", out)
	}
}

func TestCLI_RunsJSON(t *testing.T) {
	sockPath := setupMockIPCServer(t)

	out, err := executeCommand("runs", "--socket", sockPath, "--json")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	var res ipc.RunsResponse
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("saída não é JSON válido: %v\n%s", err, out)
	}
	if len(res.Runs) != 2 {
		t.Fatalf("esperava 2 execuções, obteve %d", len(res.Runs))
	}
	if res.Runs[1].ErrorStep != "git.push" {
		t.Errorf("error_step não preservado: %+v", res.Runs[1])
	}
}

func TestShortIDTruncatesGeneratedIdentifiers(t *testing.T) {
	long := "job_1788574034333973543_vault_sync"
	got := shortID(long)

	if len(got) >= len(long) {
		t.Errorf("ID longo não foi encurtado: %q", got)
	}
	if !strings.HasPrefix(got, "job_1788") {
		t.Errorf("o prefixo identificador deve ser preservado: %q", got)
	}
	// IDs curtos passam intactos
	if shortID("job_1") != "job_1" {
		t.Errorf("ID curto foi alterado: %q", shortID("job_1"))
	}
}

func TestTruncateCellFlattensNewlines(t *testing.T) {
	got := truncateCell("linha 1\nlinha 2", 100)
	if strings.Contains(got, "\n") {
		t.Errorf("quebras de linha destruiriam o alinhamento da tabela: %q", got)
	}
}
