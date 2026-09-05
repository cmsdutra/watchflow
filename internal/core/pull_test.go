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

// countJobsByPrefix separa os jobs por origem: 'pull_' vem da consulta
// periódica ao remoto, 'job_' de eventos do sistema de arquivos.
func countJobsByPrefix(t *testing.T, coord *core.Coordinator, prefix string) int {
	t.Helper()

	jobs, err := coord.Store().ListRecentJobs("", 500)
	if err != nil {
		t.Fatal(err)
	}

	n := 0
	for _, j := range jobs {
		if strings.HasPrefix(j.ID, prefix) {
			n++
		}
	}
	return n
}

func pullYAML(base, repo, interval string) string {
	return "daemon:\n  state_dir: \"" + base + "/state\"\n  socket_path: \"" + base + "/wf.sock\"\n" +
		"notifications:\n  enabled: false\n  backend: \"log\"\n" +
		"watchers:\n  - {name: vault, path: \"" + repo + "\", debounce: 200ms, max_wait: 500ms, pull_interval: \"" + interval + "\"}\n"
}

// TestPullLoopEnqueuesWithoutLocalChanges cobre a lacuna que motivou o recurso:
// sem ele, o que outra máquina publicou só chegava quando algo mudava aqui.
func TestPullLoopEnqueuesWithoutLocalChanges(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	// 30s é o mínimo aceito pela configuração; o teste força um valor menor
	// direto na struct para não precisar esperar meio minuto.
	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "30s")), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Watchers[0].PullIntervalDuration = 300 * time.Millisecond

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		sc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(sc)
	})

	// Nenhum arquivo é tocado durante a espera
	time.Sleep(1500 * time.Millisecond)

	if n := countJobsByPrefix(t, coord, "pull_"); n == 0 {
		t.Fatal("nenhuma consulta periódica foi enfileirada sem alteração local")
	}
}

func TestPullLoopDisabledByZero(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "0")), true)
	if err != nil {
		t.Fatal(err)
	}

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		sc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(sc)
	})

	time.Sleep(1200 * time.Millisecond)

	if n := countJobsByPrefix(t, coord, "pull_"); n != 0 {
		t.Errorf("pull_interval 0 deveria desativar a consulta, mas gerou %d job(s)", n)
	}
}

// TestPullLoopSkipsWhenPaused: um watcher pausado não pode acumular jobs.
func TestPullLoopSkipsWhenPaused(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "30s")), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Watchers[0].PullIntervalDuration = 250 * time.Millisecond

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		sc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(sc)
	})

	time.Sleep(200 * time.Millisecond)
	if _, err := coord.Pause(ctx, "vault"); err != nil {
		t.Fatal(err)
	}

	before := countJobsByPrefix(t, coord, "pull_")
	time.Sleep(1200 * time.Millisecond)
	after := countJobsByPrefix(t, coord, "pull_")

	if after != before {
		t.Errorf("watcher pausado não deveria acumular consultas periódicas: %d → %d", before, after)
	}
}

// TestPullLoopSkipsAfterRecentLocalActivity evita trabalho redundante enquanto o
// usuário está editando ativamente.
func TestPullLoopSkipsAfterRecentLocalActivity(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "30s")), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Watchers[0].PullIntervalDuration = 400 * time.Millisecond

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		sc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(sc)
	})

	time.Sleep(300 * time.Millisecond)

	// Edições contínuas mantêm a atividade local recente
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.WriteFile(filepath.Join(repo, "nota.md"),
			[]byte(time.Now().String()), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	local := countJobsByPrefix(t, coord, "job_")
	pull := countJobsByPrefix(t, coord, "pull_")

	if local == 0 {
		t.Fatal("as edições locais não geraram nenhum job")
	}
	// Com atividade local constante, a consulta periódica deve ser suprimida
	if pull > local {
		t.Errorf("consultas periódicas (%d) não deveriam superar os jobs locais (%d) sob edição contínua", pull, local)
	}
}

// TestPullHappensOnStartupNotAfterFullInterval cobre o cenário do boot: a
// máquina acabou de ligar e está no seu ponto de maior defasagem. Esperar um
// intervalo inteiro (5 min no padrão) para descobrir o que as outras máquinas
// publicaram é o pior momento possível — é justamente quando o usuário abre as
// notas e pode editar em cima de uma versão velha.
func TestPullHappensOnStartupNotAfterFullInterval(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	// Intervalo longo de propósito: se a primeira consulta dependesse do
	// ticker, nada aconteceria dentro do tempo deste teste.
	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "1h")), true)
	if err != nil {
		t.Fatal(err)
	}

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = coord.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		sc, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = coord.Shutdown(sc)
	})

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if countJobsByPrefix(t, coord, "pull_") > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatal("nenhuma consulta ao remoto no início; com intervalo de 1h, o usuário ficaria uma hora desatualizado após ligar a máquina")
}
