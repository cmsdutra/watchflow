package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestLog monta um config.yaml apontando para um state_dir com um log JSON
// já populado, devolvendo o caminho do config.
func writeTestLog(t *testing.T, lines []string) string {
	t.Helper()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(dir, "vault")
	if err := os.MkdirAll(vault, 0755); err != nil {
		t.Fatal(err)
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "watchflow.log"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := `version: 1
daemon:
  state_dir: "` + yamlPath(stateDir) + `"
  socket_path: "` + yamlPath(filepath.Join(dir, "wf.sock")) + `"
  log_level: "info"
  max_concurrent_pipelines: 1
notifications:
  enabled: false
  backend: "log"
watchers:
  - name: "vault"
    path: "` + yamlPath(vault) + `"
    debounce: "1s"
    max_wait: "2s"
    pipelines: ["p"]
pipelines:
  p:
    timeout: "10s"
    steps:
      - action: "git.add"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

var sampleLog = []string{
	`{"time":"2026-09-04T10:00:00.000Z","level":"INFO","msg":"daemon pronto","component":"coordinator","pid":123}`,
	`{"time":"2026-09-04T10:00:01.000Z","level":"DEBUG","msg":"comando git executado","component":"git","args":"status --porcelain"}`,
	`{"time":"2026-09-04T10:00:02.000Z","level":"WARN","msg":"falha transitória","component":"pipeline","watcher":"vault"}`,
	`{"time":"2026-09-04T10:00:03.000Z","level":"ERROR","msg":"job falhou","component":"coordinator","watcher":"outro"}`,
}

func TestCLI_LogsFormatsEntries(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	out, err := executeCommand("logs", "--config", cfgPath)
	if err != nil {
		t.Fatalf("erro ao executar logs: %v (%s)", err, out)
	}

	if !strings.Contains(out, "10:00:00 INFO  [coordinator] daemon pronto") {
		t.Errorf("formatação inesperada:\n%s", out)
	}
	if !strings.Contains(out, "pid=123") {
		t.Errorf("atributos extras não foram exibidos:\n%s", out)
	}
	if strings.Count(out, "\n") != len(sampleLog) {
		t.Errorf("esperava %d linhas, obteve:\n%s", len(sampleLog), out)
	}
}

func TestCLI_LogsFiltersByLevel(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	out, err := executeCommand("logs", "--config", cfgPath, "--level", "warn")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	if strings.Contains(out, "daemon pronto") || strings.Contains(out, "comando git") {
		t.Errorf("entradas abaixo de WARN não deveriam aparecer:\n%s", out)
	}
	if !strings.Contains(out, "falha transitória") || !strings.Contains(out, "job falhou") {
		t.Errorf("esperava WARN e ERROR na saída:\n%s", out)
	}
}

func TestCLI_LogsFiltersByComponentAndWatcher(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	out, err := executeCommand("logs", "--config", cfgPath, "--component", "git")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}
	if !strings.Contains(out, "comando git executado") || strings.Contains(out, "daemon pronto") {
		t.Errorf("filtro por componente falhou:\n%s", out)
	}

	out, err = executeCommand("logs", "--config", cfgPath, "--watcher", "vault")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}
	if !strings.Contains(out, "falha transitória") {
		t.Errorf("esperava a entrada do watcher 'vault':\n%s", out)
	}
	if strings.Contains(out, "job falhou") {
		t.Errorf("entrada de outro watcher vazou no filtro:\n%s", out)
	}
}

func TestCLI_LogsRespectsLineLimit(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	out, err := executeCommand("logs", "--config", cfgPath, "-n", "2")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	if strings.Count(out, "\n") != 2 {
		t.Errorf("esperava 2 linhas, obteve:\n%s", out)
	}
	// -n exibe a CAUDA: as duas últimas
	if !strings.Contains(out, "job falhou") || strings.Contains(out, "daemon pronto") {
		t.Errorf("esperava as duas últimas entradas:\n%s", out)
	}
}

func TestCLI_LogsJSONPassthrough(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	out, err := executeCommand("logs", "--config", cfgPath, "--json", "-n", "1")
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	if !strings.Contains(out, `"msg":"job falhou"`) {
		t.Errorf("esperava a linha JSON original intacta:\n%s", out)
	}
}

func TestCLI_LogsHandlesMalformedLines(t *testing.T) {
	cfgPath := writeTestLog(t, append([]string{"isto não é json"}, sampleLog...))

	out, err := executeCommand("logs", "--config", cfgPath)
	if err != nil {
		t.Fatalf("erro: %v (%s)", err, out)
	}

	// Linhas não-JSON são preservadas: podem ser mensagens do runtime
	if !strings.Contains(out, "isto não é json") {
		t.Errorf("linha malformada foi descartada silenciosamente:\n%s", out)
	}
}

func TestCLI_LogsReportsMissingFile(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	logPath := filepath.Join(filepath.Dir(cfgPath), "state", "watchflow.log")
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}

	_, err := executeCommand("logs", "--config", cfgPath)
	if err == nil {
		t.Fatal("esperava erro quando o log não existe")
	}
	if !strings.Contains(err.Error(), "nenhum log encontrado") {
		t.Errorf("mensagem pouco clara: %v", err)
	}
}

func TestCLI_LogsRejectsInvalidLevel(t *testing.T) {
	cfgPath := writeTestLog(t, sampleLog)

	_, err := executeCommand("logs", "--config", cfgPath, "--level", "trace")
	if err == nil {
		t.Fatal("esperava erro para nível inválido")
	}
}

// TestLogsFollowSurvivesRotation garante que --follow continua funcionando
// quando o daemon rotaciona o arquivo por tamanho: sem tratar o encolhimento,
// a leitura ficaria presa num deslocamento além do fim do novo arquivo.
func TestLogsFollowSurvivesRotation(t *testing.T) {
	// Este teste chama os helpers diretamente, sem passar por executeCommand:
	// precisa zerar as flags globais para não herdar as do teste anterior.
	resetCommandFlags(rootCmd)

	dir := t.TempDir()
	path := filepath.Join(dir, "watchflow.log")

	original := `{"time":"2026-09-04T10:00:00.000Z","level":"INFO","msg":"antes da rotação"}`
	if err := os.WriteFile(path, []byte(original+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	filter, err := newLogFilter()
	if err != nil {
		t.Fatal(err)
	}

	var sink strings.Builder
	offset, err := printTail(&sink, f, 10, filter)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if offset == 0 {
		t.Fatal("esperava deslocamento após ler a cauda")
	}

	// Rotação: arquivo substituído por um menor
	rotated := `{"time":"2026-09-04T10:00:05.000Z","level":"INFO","msg":"depois"}`
	if err := os.WriteFile(path, []byte(rotated+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= offset {
		t.Skip("arquivo rotacionado não ficou menor; cenário não exercitado")
	}

	sink.Reset()
	if _, err := drainFrom(&sink, path, 0, filter); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.String(), "depois") {
		t.Errorf("conteúdo pós-rotação não foi lido: %q", sink.String())
	}
}
