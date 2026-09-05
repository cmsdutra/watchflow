package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/queue"
)

// fakeClient registra as chamadas recebidas para verificar que os atalhos
// disparam a ação correta no daemon.
type fakeClient struct {
	mu sync.Mutex

	status *ipc.StatusResponse
	jobs   []ipc.JobDTO
	err    error

	syncedWith  []string
	pausedWith  []string
	resumedWith []string
}

func (c *fakeClient) Status(context.Context) (*ipc.StatusResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return c.status, nil
}

func (c *fakeClient) Jobs(_ context.Context, _ string, _ int) (*ipc.JobsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return &ipc.JobsResponse{Jobs: c.jobs}, nil
}

func (c *fakeClient) Sync(_ context.Context, name string) (*ipc.SyncResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncedWith = append(c.syncedWith, name)
	return &ipc.SyncResponse{Message: "sincronização solicitada"}, nil
}

func (c *fakeClient) Pause(_ context.Context, name string) (*ipc.ActionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pausedWith = append(c.pausedWith, name)
	return &ipc.ActionResponse{Success: true, Message: "pausado"}, nil
}

func (c *fakeClient) Resume(_ context.Context, name string) (*ipc.ActionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumedWith = append(c.resumedWith, name)
	return &ipc.ActionResponse{Success: true, Message: "retomado"}, nil
}

func (c *fakeClient) calls() ([]string, []string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.syncedWith...), append([]string{}, c.pausedWith...), append([]string{}, c.resumedWith...)
}

type fakeEvents struct{ entries []logger.Entry }

func (e fakeEvents) Recent(int) ([]logger.Entry, error) { return e.entries, nil }

func sampleStatus() *ipc.StatusResponse {
	return &ipc.StatusResponse{
		DaemonPID: 4242, Uptime: "1h2m", Version: "1.0.0",
		PendingJobs: 2, RunningJobs: 1, BlockedJobs: 1,
		Watchers: []ipc.WatcherStatusDTO{
			{Name: "vault-a", Path: "/home/u/A", Status: string(queue.WatcherHealthy), LastSuccessSyncAt: "2026-09-04 10:00:00"},
			{Name: "vault-b", Path: "/home/u/B", Status: string(queue.WatcherPaused)},
			{Name: "vault-c", Path: "/home/u/C", Status: string(queue.WatcherConflictHalted), LastError: "conflito de merge detectado"},
		},
	}
}

// newLoadedModel devolve um modelo já com dados, como após o primeiro refresh.
func newLoadedModel(t *testing.T, c *fakeClient) Model {
	t.Helper()

	m := New(Options{Client: c, Events: fakeEvents{}, Version: "1.0.0"})
	updated, _ := m.Update(statusMsg{status: c.status})
	return updated.(Model)
}

// drain executa os comandos que o Update devolveu, entregando as mensagens
// resultantes de volta ao modelo — equivalente ao que o runtime do Bubble Tea
// faz, mas de forma síncrona e determinística.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}

	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			m = drain(t, m, sub)
		}
		return m
	}

	updated, _ := m.Update(msg)
	return updated.(Model)
}

func TestNavigationMovesCursorWithinBounds(t *testing.T) {
	c := &fakeClient{status: sampleStatus()}
	m := newLoadedModel(t, c)

	if m.cursor != 0 {
		t.Fatalf("cursor inicial deveria ser 0, é %d", m.cursor)
	}

	// Sobe no topo: não pode ficar negativo
	up, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if up.(Model).cursor != 0 {
		t.Errorf("cursor saiu do limite superior: %d", up.(Model).cursor)
	}

	for i := 0; i < 10; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = next.(Model)
	}
	if m.cursor != 2 {
		t.Errorf("cursor deveria parar no último watcher (2), está em %d", m.cursor)
	}
}

func TestSyncShortcutTargetsSelectedWatcher(t *testing.T) {
	c := &fakeClient{status: sampleStatus()}
	m := newLoadedModel(t, c)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(Model)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	drain(t, next.(Model), cmd)

	synced, _, _ := c.calls()
	if len(synced) != 1 || synced[0] != "vault-b" {
		t.Errorf("esperava sync para 'vault-b', obteve %v", synced)
	}
}

// TestPauseKeyTogglesByCurrentState cobre o comportamento de tecla única: 'p'
// pausa o que está ativo e retoma o que está parado — inclusive o watcher
// interrompido por conflito, cujo destravamento é justamente o 'resume'.
func TestPauseKeyTogglesByCurrentState(t *testing.T) {
	cases := []struct {
		name        string
		cursor      int
		wantPause   string
		wantResume  string
		description string
	}{
		{"watcher saudável é pausado", 0, "vault-a", "", "HEALTHY"},
		{"watcher pausado é retomado", 1, "", "vault-b", "PAUSED"},
		{"watcher em conflito é retomado", 2, "", "vault-c", "CONFLICT_HALTED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeClient{status: sampleStatus()}
			m := newLoadedModel(t, c)
			m.cursor = tc.cursor

			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
			drain(t, next.(Model), cmd)

			_, paused, resumed := c.calls()

			if tc.wantPause != "" {
				if len(paused) != 1 || paused[0] != tc.wantPause {
					t.Errorf("%s: esperava pause em %q, obteve %v", tc.description, tc.wantPause, paused)
				}
			} else if len(paused) != 0 {
				t.Errorf("%s: não deveria pausar, obteve %v", tc.description, paused)
			}

			if tc.wantResume != "" {
				if len(resumed) != 1 || resumed[0] != tc.wantResume {
					t.Errorf("%s: esperava resume em %q, obteve %v", tc.description, tc.wantResume, resumed)
				}
			} else if len(resumed) != 0 {
				t.Errorf("%s: não deveria retomar, obteve %v", tc.description, resumed)
			}
		})
	}
}

func TestTabCyclesFocus(t *testing.T) {
	c := &fakeClient{status: sampleStatus()}
	m := newLoadedModel(t, c)

	want := []Panel{PanelJobs, PanelAudit, PanelWatchers}
	for i, expected := range want {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
		if m.focus != expected {
			t.Errorf("passo %d: foco = %s, esperado %s", i, m.focus.Name(), expected.Name())
		}
	}

	// Shift+Tab volta
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if next.(Model).focus != PanelAudit {
		t.Errorf("shift+tab deveria voltar para auditoria, foi para %s", next.(Model).focus.Name())
	}
}

func TestQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyEsc},
	} {
		m := New(Options{Client: &fakeClient{}, Events: fakeEvents{}})
		next, cmd := m.Update(key)
		if !next.(Model).quitting {
			t.Errorf("tecla %v deveria encerrar a TUI", key)
		}
		if cmd == nil {
			t.Errorf("tecla %v deveria devolver tea.Quit", key)
		}
	}
}

// TestDaemonFailureKeepsUIAlive garante que a TUI não morre nem trava quando o
// daemon está fora do ar: ele pode estar reiniciando via systemd.
func TestDaemonFailureKeepsUIAlive(t *testing.T) {
	m := New(Options{Client: &fakeClient{}, Events: fakeEvents{}})

	next, _ := m.Update(statusMsg{err: errors.New("dial unix: connection refused")})
	m = next.(Model)

	if m.connErr == nil {
		t.Fatal("erro de conexão não foi registrado")
	}
	if m.quitting {
		t.Error("a TUI não deveria encerrar por falha de conexão")
	}

	view := m.View()
	if !strings.Contains(view, "Não foi possível falar com o daemon") {
		t.Errorf("a tela deveria explicar a desconexão:\n%s", view)
	}
	if !strings.Contains(view, "connection refused") {
		t.Errorf("a tela deveria mostrar a causa:\n%s", view)
	}

	// Reconectou: o estado volta ao normal
	next, _ = m.Update(statusMsg{status: sampleStatus()})
	if next.(Model).connErr != nil {
		t.Error("o erro deveria ser limpo ao reconectar")
	}
}

func TestCursorClampsWhenWatchersDisappear(t *testing.T) {
	c := &fakeClient{status: sampleStatus()}
	m := newLoadedModel(t, c)
	m.cursor = 2

	// Configuração recarregada com menos watchers
	shrunk := sampleStatus()
	shrunk.Watchers = shrunk.Watchers[:1]

	next, _ := m.Update(statusMsg{status: shrunk})
	if got := next.(Model).cursor; got != 0 {
		t.Errorf("cursor deveria ser reajustado para 0, está em %d", got)
	}
}

func TestViewRendersAllPanels(t *testing.T) {
	c := &fakeClient{
		status: sampleStatus(),
		jobs: []ipc.JobDTO{
			{ID: "j1", WatcherID: "vault-c", PipelineName: "sync", Status: string(queue.StatusBlocked),
				RetryCount: 0, MaxRetries: 5, LastError: "conflito de merge detectado"},
			{ID: "j2", WatcherID: "vault-a", PipelineName: "sync", Status: string(queue.StatusPending),
				RetryCount: 1, MaxRetries: 5, ScheduledFor: "2026-09-04 10:05:00"},
		},
	}

	m := newLoadedModel(t, c)
	m, _ = mustUpdate(m, jobsMsg{jobs: c.jobs})
	m, _ = mustUpdate(m, eventsMsg{entries: []logger.Entry{
		{Time: time.Now(), Level: "INFO", Component: "pipeline", Message: "pipeline concluído",
			Attrs: map[string]any{"duracao": "1.2s"}},
		{Time: time.Now(), Level: "ERROR", Component: "coordinator", Message: "job falhou",
			Attrs: map[string]any{"step": "git.push"}},
	}})

	view := m.View()

	for _, want := range []string{
		"WatchFlow Monitor v1.0.0",
		"PID 4242",
		"COFRES VIGIADOS",
		"vault-a", "vault-b", "vault-c",
		"CONFLICT_HALTED",
		"FILA PERSISTENTE",
		"AUDITORIA E EVENTOS RECENTES",
		"pipeline concluído",
		"[s] sincronizar",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("a tela deveria conter %q:\n%s", want, view)
		}
	}
}

// TestBlockedJobsAppearFirst confirma a priorização da fila: o que exige
// intervenção humana não pode ficar soterrado sob dezenas de pendentes.
func TestBlockedJobsAppearFirst(t *testing.T) {
	jobs := []ipc.JobDTO{
		{ID: "p1", Status: string(queue.StatusPending), PipelineName: "sync"},
		{ID: "c1", Status: string(queue.StatusCompleted), PipelineName: "sync"},
		{ID: "b1", Status: string(queue.StatusBlocked), PipelineName: "sync"},
		{ID: "r1", Status: string(queue.StatusRunning), PipelineName: "sync"},
	}

	got := activeJobs(jobs)
	if len(got) != 3 {
		t.Fatalf("jobs concluídos não deveriam ocupar a fila: %d entradas", len(got))
	}
	if got[0].Status != string(queue.StatusBlocked) {
		t.Errorf("bloqueado deveria vir primeiro, veio %s", got[0].Status)
	}
	if got[1].Status != string(queue.StatusRunning) {
		t.Errorf("em execução deveria vir em segundo, veio %s", got[1].Status)
	}
}

func TestEventLineIncludesRelevantDetail(t *testing.T) {
	line := eventLine(logger.Entry{
		Message: "step concluído",
		Attrs: map[string]any{
			"step":     "git.commit",
			"duracao":  "31ms",
			"arquivos": "2",
		},
	})

	for _, want := range []string{"step concluído", "git.commit", "31ms", "2 arquivo(s)"} {
		if !strings.Contains(line, want) {
			t.Errorf("linha de evento deveria conter %q: %s", want, line)
		}
	}
}

func TestRelevantEventsDropsUnrelatedDebug(t *testing.T) {
	entries := []logger.Entry{
		{Level: "DEBUG", Component: "ipc", Message: "requisição IPC recebida"},
		{Level: "DEBUG", Component: "git", Message: "comando git executado"},
		{Level: "INFO", Component: "coordinator", Message: "lote consolidado"},
	}

	got := relevantEvents(entries, 10)
	if len(got) != 2 {
		t.Fatalf("esperava 2 eventos relevantes, obteve %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Component == "ipc" {
			t.Error("ruído de debug do IPC não deveria aparecer na auditoria")
		}
	}
}

func TestTruncateHandlesMultibyte(t *testing.T) {
	// Truncar por bytes quebraria caracteres acentuados no meio
	got := truncate("sincronização", 8)
	if len([]rune(got)) != 8 {
		t.Errorf("esperava 8 runes, obteve %d (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("esperava reticências no corte: %q", got)
	}
	if unchanged := truncate("curto", 40); unchanged != "curto" {
		t.Errorf("texto curto foi alterado: %q", unchanged)
	}
}

func mustUpdate(m Model, msg tea.Msg) (Model, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(Model), cmd
}
