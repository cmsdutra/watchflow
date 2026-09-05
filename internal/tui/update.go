package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/queue"
)

// callTimeout limita cada chamada IPC para que o daemon travado não congele a
// interface — a TUI precisa continuar respondendo ao teclado.
const callTimeout = 3 * time.Second

// Update processa eventos do terminal e respostas assíncronas do daemon.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		return m, tea.Batch(m.refreshAll(), m.scheduleTick())

	case statusMsg:
		if msg.err != nil {
			m.connErr = msg.err
			return m, nil
		}
		m.connErr = nil
		m.status = msg.status
		m.clampCursor()
		return m, nil

	case jobsMsg:
		if msg.err == nil {
			m.jobs = msg.jobs
		}
		return m, nil

	case eventsMsg:
		m.entries = msg.entries
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m = m.withFeedback(msg.err.Error(), feedbackError)
		} else {
			m = m.withFeedback(msg.message, feedbackInfo)
		}
		// Reflete o efeito da ação imediatamente, sem esperar o próximo tick
		return m, m.refreshAll()
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit

	case "tab":
		m.focus = (m.focus + 1) % panelCount
		return m, nil

	case "shift+tab":
		m.focus = (m.focus + panelCount - 1) % panelCount
		return m, nil

	case "up", "k":
		if m.focus == PanelWatchers && m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.focus == PanelWatchers && m.cursor < m.watcherCount()-1 {
			m.cursor++
		}
		return m, nil

	case "r":
		return m.withFeedback("atualizando…", feedbackInfo), m.refreshAll()

	case "s":
		w, ok := m.selectedWatcher()
		if !ok {
			return m.withFeedback("nenhum watcher selecionado", feedbackError), nil
		}
		return m, m.syncCmd(w.Name)

	case "p":
		w, ok := m.selectedWatcher()
		if !ok {
			return m.withFeedback("nenhum watcher selecionado", feedbackError), nil
		}
		// Uma única tecla alterna: pausar o que está ativo, retomar o que está
		// pausado ou interrompido por conflito.
		if isHalted(w.Status) {
			return m, m.resumeCmd(w.Name)
		}
		return m, m.pauseCmd(w.Name)
	}

	return m, nil
}

// isHalted informa se o watcher está parado e, portanto, se 'p' deve retomá-lo.
func isHalted(status string) bool {
	return status == string(queue.WatcherPaused) || status == string(queue.WatcherConflictHalted)
}

func (m Model) withFeedback(text string, kind feedbackKind) Model {
	m.feedback = text
	m.feedbackKind = kind
	m.feedbackTill = time.Now().Add(feedbackDuration)
	return m
}

// clampCursor mantém o cursor dentro da lista quando watchers somem da config.
func (m *Model) clampCursor() {
	if n := m.watcherCount(); m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m Model) scheduleTick() tea.Cmd {
	return tea.Tick(m.refreshEvery, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// refreshAll dispara as três cargas em paralelo; o Bubble Tea entrega cada
// resposta como uma mensagem independente.
func (m Model) refreshAll() tea.Cmd {
	return tea.Batch(m.statusCmd(), m.jobsCmd(), m.eventsCmd())
}

func (m Model) statusCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		res, err := client.Status(ctx)
		return statusMsg{status: res, err: err}
	}
}

func (m Model) jobsCmd() tea.Cmd {
	client := m.client
	limit := m.jobLimit
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		res, err := client.Jobs(ctx, "", limit)
		if err != nil {
			return jobsMsg{err: err}
		}
		return jobsMsg{jobs: res.Jobs}
	}
}

func (m Model) eventsCmd() tea.Cmd {
	source := m.events
	limit := m.eventLimit
	return func() tea.Msg {
		if source == nil {
			return eventsMsg{}
		}
		// Log ausente ou ilegível não é erro fatal para a TUI: o painel de
		// auditoria simplesmente fica vazio.
		entries, err := source.Recent(limit)
		if err != nil {
			return eventsMsg{}
		}
		return eventsMsg{entries: entries}
	}
}

func (m Model) syncCmd(watcher string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		res, err := client.Sync(ctx, watcher)
		if err != nil {
			return actionMsg{err: fmt.Errorf("falha ao sincronizar '%s': %w", watcher, err)}
		}
		return actionMsg{message: res.Message}
	}
}

func (m Model) pauseCmd(watcher string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		res, err := client.Pause(ctx, watcher)
		return actionResult(res, err, "pausar", watcher)
	}
}

func (m Model) resumeCmd(watcher string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()

		res, err := client.Resume(ctx, watcher)
		return actionResult(res, err, "retomar", watcher)
	}
}

func actionResult(res *ipc.ActionResponse, err error, verb, watcher string) tea.Msg {
	if err != nil {
		return actionMsg{err: fmt.Errorf("falha ao %s '%s': %w", verb, watcher, err)}
	}
	return actionMsg{message: res.Message}
}
