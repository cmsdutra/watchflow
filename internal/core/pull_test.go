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
	return "daemon:\n  state_dir: \"" + yamlPath(base) + "/state\"\n  socket_path: \"" + yamlPath(base) + "/wf.sock\"\n" +
		"notifications:\n  enabled: false\n  backend: \"log\"\n" +
		"watchers:\n  - {name: vault, path: \"" + yamlPath(repo) + "\", debounce: 200ms, max_wait: 500ms, pull_interval: \"" + interval + "\"}\n"
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
//
// Os parâmetros precisam expressar a intenção: a sincronização local tem de ser
// mais frequente que o pull_interval, senão sobram frestas entre uma alteração e
// a seguinte em que o tick legitimamente não é suprimido. Com o max_wait de
// 500ms acima do pull_interval de 400ms usados antes, essas frestas existiam e o
// teste só passava porque o próprio laço de pull se auto-suprimia — ou seja,
// media o bug de cadência, não o guard.
func TestPullLoopSkipsAfterRecentLocalActivity(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "30s")), true)
	if err != nil {
		t.Fatal(err)
	}
	// Janela de supressão (600ms) bem acima da cadência de flush local (~150ms):
	// toda consulta periódica cai dentro do rastro de uma alteração local.
	cfg.Watchers[0].DebounceDuration = 100 * time.Millisecond
	cfg.Watchers[0].MaxWaitDuration = 200 * time.Millisecond
	cfg.Watchers[0].PullIntervalDuration = 600 * time.Millisecond

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
	// Sob edição contínua, praticamente todo tick deve ser suprimido. Comparar
	// com o número de jobs locais é frouxo demais: passaria com o guard inerte.
	const toleradas = 1
	if pull > toleradas {
		t.Errorf("esperava no máximo %d consulta periódica sob edição contínua (%d jobs locais), obteve %d",
			toleradas, local, pull)
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

// TestPullLoopRespeitaCadenciaConfigurada é regressão de um bug em que a
// consulta periódica rodava na METADE da frequência configurada.
//
// O laço carimbava lastEnqueue a cada consulta, e shouldSkipPull pula quando a
// última sincronização é mais recente que pull_interval. Como o ticker tem
// exatamente o período da janela e o carimbo acontece microssegundos depois do
// tick, o tick seguinte sempre encontrava time.Since() logo abaixo do limite e
// se pulava — alternando tick/pulo indefinidamente.
//
// O teste conta consultas numa janela de vários períodos: com o bug presente
// o número cai para cerca de metade do esperado.
func TestPullLoopRespeitaCadenciaConfigurada(t *testing.T) {
	base := t.TempDir()
	repo := fakeRepo(t, base, "vault")

	cfg, err := config.LoadBytes([]byte(pullYAML(base, repo, "30s")), true)
	if err != nil {
		t.Fatal(err)
	}
	const interval = 200 * time.Millisecond
	cfg.Watchers[0].PullIntervalDuration = interval

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

	// Nenhum arquivo é tocado: toda consulta no período vem do ticker.
	const janela = 10 * interval
	time.Sleep(janela + 300*time.Millisecond)

	got := countJobsByPrefix(t, coord, "pull_")

	// Margem generosa para agendamento do runtime, mas abaixo dela só se passa
	// com o tick alternado do bug (que renderia ~5-6).
	const minimo = 8
	if got < minimo {
		t.Errorf("esperava ao menos %d consultas em %s com pull_interval de %s, obteve %d "+
			"(cadência real parece ser o dobro do configurado)", minimo, janela, interval, got)
	}
}
