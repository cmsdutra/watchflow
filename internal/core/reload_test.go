package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
)

// fakeRepo cria uma pasta que passa pela validação de repositório Git.
func fakeRepo(t *testing.T, base, name string) string {
	t.Helper()

	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n[remote \"origin\"]\n\turl = https://example.com/r.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setupReloadDaemon sobe um coordenador real a partir de um arquivo de
// configuração em disco, que é o que o reload relê.
func setupReloadDaemon(t *testing.T, yaml string) (*core.Coordinator, string, func()) {
	t.Helper()

	base := t.TempDir()
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("configuração inicial inválida: %v", err)
	}

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	coord.SetConfigPath(cfgPath)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	time.Sleep(300 * time.Millisecond)

	cleanup := func() {
		cancel()
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(shutdownCtx)
	}
	return coord, cfgPath, cleanup
}

func daemonYAML(base string, watchers string) string {
	return "daemon:\n  state_dir: \"" + yamlPath(base) + "/state\"\n  socket_path: \"" + yamlPath(base) + "/wf.sock\"\n" +
		"notifications:\n  enabled: false\n  backend: \"log\"\n" +
		"watchers:\n" + watchers
}

func activeWatchers(t *testing.T, coord *core.Coordinator) []string {
	t.Helper()

	res, err := coord.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range res.Watchers {
		names = append(names, w.Name)
	}
	return names
}

// TestReloadAddsRemovesAndUpdatesWatchers cobre o caso central: mudar a
// configuração sem reiniciar o daemon.
func TestReloadAddsRemovesAndUpdatesWatchers(t *testing.T) {
	base := t.TempDir()
	repoA := fakeRepo(t, base, "A")
	repoB := fakeRepo(t, base, "B")

	yaml := daemonYAML(base, "  - {name: vault-a, path: \""+yamlPath(repoA)+"\", debounce: 300ms, max_wait: 1s}\n")

	coord, cfgPath, cleanup := setupReloadDaemon(t, yaml)
	defer cleanup()

	// Adiciona B e muda a janela de A
	updated := daemonYAML(base,
		"  - {name: vault-a, path: \""+yamlPath(repoA)+"\", debounce: 900ms, max_wait: 2s}\n"+
			"  - {name: vault-b, path: \""+yamlPath(repoB)+"\", debounce: 300ms, max_wait: 1s}\n")
	if err := os.WriteFile(cfgPath, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := coord.Reload(context.Background())
	if err != nil {
		t.Fatalf("reload falhou: %v", err)
	}
	if !res.Success {
		t.Errorf("reload reportou falha: %+v", res)
	}
	if len(res.WatchersAdded) != 1 || res.WatchersAdded[0] != "vault-b" {
		t.Errorf("esperava vault-b adicionado, obteve %v", res.WatchersAdded)
	}
	if len(res.WatchersUpdated) != 1 || res.WatchersUpdated[0] != "vault-a" {
		t.Errorf("esperava vault-a atualizado (janela mudou), obteve %v", res.WatchersUpdated)
	}

	// Remove A
	onlyB := daemonYAML(base, "  - {name: vault-b, path: \""+yamlPath(repoB)+"\", debounce: 300ms, max_wait: 1s}\n")
	if err := os.WriteFile(cfgPath, []byte(onlyB), 0600); err != nil {
		t.Fatal(err)
	}

	res, err = coord.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.WatchersRemoved) != 1 || res.WatchersRemoved[0] != "vault-a" {
		t.Errorf("esperava vault-a removido, obteve %v", res.WatchersRemoved)
	}
}

// TestReloadRejectsInvalidConfigWithoutChangingAnything: recusar é fundamental —
// aplicar uma configuração quebrada pela metade pararia a sincronização.
func TestReloadRejectsInvalidConfig(t *testing.T) {
	base := t.TempDir()
	repoA := fakeRepo(t, base, "A")

	yaml := daemonYAML(base, "  - {name: vault-a, path: \""+yamlPath(repoA)+"\", debounce: 300ms, max_wait: 1s}\n")
	coord, cfgPath, cleanup := setupReloadDaemon(t, yaml)
	defer cleanup()

	before := activeWatchers(t, coord)

	broken := daemonYAML(base, "  - {name: vault-a, path: \""+yamlPath(repoA)+"\", pipelines: [inexistente]}\n")
	if err := os.WriteFile(cfgPath, []byte(broken), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := coord.Reload(context.Background()); err == nil {
		t.Fatal("esperava erro para configuração inválida")
	} else if !strings.Contains(err.Error(), "nada foi alterado") {
		t.Errorf("a mensagem deveria deixar claro que nada mudou: %v", err)
	}

	if after := activeWatchers(t, coord); len(after) != len(before) {
		t.Errorf("o daemon deveria continuar com a configuração anterior: antes=%v depois=%v", before, after)
	}
}

// TestReloadReportsDaemonChangesAsNeedingRestart: essas mudanças não podem ser
// aplicadas com o processo no ar, e silenciá-las enganaria o usuário.
func TestReloadReportsDaemonChangesAsNeedingRestart(t *testing.T) {
	base := t.TempDir()
	repoA := fakeRepo(t, base, "A")

	yaml := daemonYAML(base, "  - {name: vault-a, path: \""+yamlPath(repoA)+"\", debounce: 300ms, max_wait: 1s}\n")
	coord, cfgPath, cleanup := setupReloadDaemon(t, yaml)
	defer cleanup()

	changed := "daemon:\n  state_dir: \"" + yamlPath(base) + "/state\"\n  socket_path: \"" + yamlPath(base) + "/wf.sock\"\n" +
		"  log_level: debug\n  max_concurrent_pipelines: 8\n" +
		"notifications:\n  enabled: false\n  backend: \"log\"\n" +
		"watchers:\n  - {name: vault-a, path: \"" + yamlPath(repoA) + "\", debounce: 300ms, max_wait: 1s}\n"
	if err := os.WriteFile(cfgPath, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := coord.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NeedsRestart) != 2 {
		t.Fatalf("esperava 2 itens exigindo reinício, obteve %v", res.NeedsRestart)
	}

	joined := strings.Join(res.NeedsRestart, " ")
	for _, want := range []string{"log_level", "max_concurrent_pipelines"} {
		if !strings.Contains(joined, want) {
			t.Errorf("esperava %q entre os itens que exigem reinício: %v", want, res.NeedsRestart)
		}
	}
}

func TestReloadSurfacesRepoWarnings(t *testing.T) {
	base := t.TempDir()

	// Repositório sem remote: válido, mas quase certamente não é o pretendido
	repo := filepath.Join(base, "local")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	yaml := daemonYAML(base, "  - {name: local, path: \""+yamlPath(repo)+"\", debounce: 300ms, max_wait: 1s}\n")
	coord, _, cleanup := setupReloadDaemon(t, yaml)
	defer cleanup()

	res, err := coord.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "remote") {
		t.Errorf("esperava aviso sobre remote ausente, obteve %v", res.Warnings)
	}
}

func TestReloadWithoutConfigPathFails(t *testing.T) {
	cfg, _ := newTestConfig(t, "noop", []config.Step{})

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = coord.Shutdown(ctx)
	})

	if _, err := coord.Reload(context.Background()); err == nil {
		t.Error("esperava erro quando o caminho da configuração é desconhecido")
	}
}
