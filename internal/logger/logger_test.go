package logger_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/logger"
)

func readLogLines(t *testing.T, dir string) []map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, logger.LogFileName))
	if err != nil {
		t.Fatalf("falha ao ler arquivo de log: %v", err)
	}

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("linha de log não é JSON válido (%q): %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func TestSetupWritesStructuredJSON(t *testing.T) {
	dir := t.TempDir()

	closer, err := logger.Setup(logger.Options{Level: "info", Dir: dir})
	if err != nil {
		t.Fatalf("falha ao configurar logger: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	logger.For("watcher").Info("watcher ativo", slog.String("watcher", "vault"), slog.Int("diretorios", 12))

	entries := readLogLines(t, dir)
	if len(entries) != 1 {
		t.Fatalf("esperava 1 linha de log, obteve %d", len(entries))
	}

	got := entries[0]
	if got["msg"] != "watcher ativo" {
		t.Errorf("mensagem inesperada: %v", got["msg"])
	}
	if got["component"] != "watcher" {
		t.Errorf("esperava component='watcher', obteve %v", got["component"])
	}
	if got["watcher"] != "vault" {
		t.Errorf("esperava watcher='vault', obteve %v", got["watcher"])
	}
	if got["level"] != "INFO" {
		t.Errorf("esperava level='INFO', obteve %v", got["level"])
	}
}

func TestLevelFiltering(t *testing.T) {
	dir := t.TempDir()

	closer, err := logger.Setup(logger.Options{Level: "warn", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	log := logger.For("teste")
	log.Debug("nao deve aparecer")
	log.Info("nao deve aparecer")
	log.Warn("deve aparecer")
	log.Error("deve aparecer")

	entries := readLogLines(t, dir)
	if len(entries) != 2 {
		t.Fatalf("esperava 2 linhas acima de WARN, obteve %d", len(entries))
	}
}

func TestParseLevelRejectsInvalid(t *testing.T) {
	if _, err := logger.ParseLevel("trace"); err == nil {
		t.Error("esperava erro para nível 'trace'")
	}
	for _, valid := range []string{"debug", "info", "warn", "error", "", "INFO"} {
		if _, err := logger.ParseLevel(valid); err != nil {
			t.Errorf("nível '%s' deveria ser aceito: %v", valid, err)
		}
	}
}

// TestSecretsAreRedactedInLogs cobre a invariante de supressão de segredos (README, "Invariantes de engenharia"): a partir do
// momento em que o daemon grava logs em disco, credenciais embutidas em URLs de
// remote e tokens de acesso não podem vazar em nenhum campo.
func TestSecretsAreRedactedInLogs(t *testing.T) {
	dir := t.TempDir()

	closer, err := logger.Setup(logger.Options{Level: "debug", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	gitErr := errors.New("fatal: unable to access 'https://ghp_secr3tToken1234567890@github.com/org/repo.git/'")

	logger.For("git").
		With(slog.String("remote", "https://user:superSecretPass@gitlab.com/vault.git")).
		Error("git push falhou",
			slog.Any("error", gitErr),
			slog.String("msg_bruta", "token github_pat_11ABCDEFG0123456789xyz vazando"),
			slog.Group("detalhe", slog.String("url", "https://oauth2:hunter2@bitbucket.org/x.git")))

	raw, err := os.ReadFile(filepath.Join(dir, logger.LogFileName))
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	forbidden := []string{
		"ghp_secr3tToken1234567890",
		"superSecretPass",
		"github_pat_11ABCDEFG0123456789xyz",
		"hunter2",
	}
	for _, secret := range forbidden {
		if strings.Contains(content, secret) {
			t.Errorf("segredo %q vazou no log:\n%s", secret, content)
		}
	}

	// O contexto útil deve sobreviver à supressão
	if !strings.Contains(content, "github.com/org/repo.git") {
		t.Errorf("a supressão removeu contexto útil demais:\n%s", content)
	}
	if !strings.Contains(content, "***") {
		t.Errorf("esperava marcador de supressão no log:\n%s", content)
	}
}

func TestRedactPreservesNonSecrets(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"nenhum segredo aqui":      "nenhum segredo aqui",
		"https://github.com/o/r":   "https://github.com/o/r",
		"git@github.com:org/r.git": "git@github.com:org/r.git",
	}
	for in, want := range cases {
		if got := logger.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, esperado %q", in, got, want)
		}
	}
}

// TestLogRotation garante que o daemon não enche o disco em execução contínua.
func TestLogRotation(t *testing.T) {
	dir := t.TempDir()

	closer, err := logger.Setup(logger.Options{Level: "info", Dir: dir, MaxSizeBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	log := logger.For("carga")
	padding := strings.Repeat("x", 200)
	for i := 0; i < 200; i++ {
		log.Info("linha de carga", slog.Int("i", i), slog.String("pad", padding))
	}

	primary := filepath.Join(dir, logger.LogFileName)
	backup := primary + ".1"

	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("esperava arquivo rotacionado '%s': %v", backup, err)
	}

	info, err := os.Stat(primary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 8192 {
		t.Errorf("arquivo de log corrente cresceu além do limite de rotação: %d bytes", info.Size())
	}
	// No Windows os bits Unix são no-op: o arquivo herda a ACL do diretório e o
	// Go reporta 0666. A restrição de acesso ao log é, nessa plataforma, uma
	// diferença documentada no README ("Limitações conhecidas").
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("esperava permissão 0600 no log (pode conter caminhos privados), obteve %o", perm)
		}
	}
}

func TestSetupWithoutDirDoesNotCreateFile(t *testing.T) {
	closer, err := logger.Setup(logger.Options{Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Error("esperava closer nulo quando não há saída em arquivo")
		_ = closer.Close()
	}

	// Não deve entrar em pânico sem destino configurado
	logger.For("x").Info("mensagem sem destino")
}

func ExampleFor() {
	logger.Discard()
	logger.For("exemplo").Info("mensagem", slog.String("chave", "valor"))
	fmt.Println("ok")
	// Output: ok
}

func TestTailReturnsLastEntriesParsed(t *testing.T) {
	dir := t.TempDir()
	closer, err := logger.Setup(logger.Options{Level: "debug", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })

	log := logger.For("pipeline")
	for i := 0; i < 10; i++ {
		log.Info("passo", slog.Int("n", i), slog.Duration("duracao", time.Duration(i)*time.Second))
	}

	entries, err := logger.Tail(filepath.Join(dir, logger.LogFileName), 3)
	if err != nil {
		t.Fatalf("Tail falhou: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("esperava as 3 últimas entradas, obteve %d", len(entries))
	}

	last := entries[2]
	if last.Component != "pipeline" || last.Message != "passo" {
		t.Errorf("campos fixos não foram extraídos: %+v", last)
	}
	if last.Attr("n") != "9" {
		t.Errorf("esperava o último registro (n=9), obteve n=%s", last.Attr("n"))
	}
	// Durações são gravadas legíveis pelo ReplaceAttr
	if last.Attr("duracao") != "9s" {
		t.Errorf("duração não legível: %q", last.Attr("duracao"))
	}
	if last.Time.IsZero() {
		t.Error("timestamp não foi decodificado")
	}
	// A exibição usa o fuso local, não o UTC em que o log é gravado
	if last.Time.Location() != time.Local {
		t.Errorf("esperava horário local, obteve %s", last.Time.Location())
	}
}

func TestTailSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logger.LogFileName)

	content := `{"time":"2026-09-04T10:00:00Z","level":"INFO","msg":"ok"}
isto não é json
{"time":"2026-09-04T10:00:01Z","level":"WARN","msg":"aviso"}
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	entries, err := logger.Tail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("esperava 2 entradas válidas, obteve %d", len(entries))
	}
}

func TestTailOnMissingFile(t *testing.T) {
	if _, err := logger.Tail(filepath.Join(t.TempDir(), "inexistente.log"), 5); err == nil {
		t.Error("esperava erro para arquivo inexistente")
	}
	if entries, err := logger.Tail("qualquer", 0); err != nil || entries != nil {
		t.Error("limite zero deveria devolver nil sem erro")
	}
}
