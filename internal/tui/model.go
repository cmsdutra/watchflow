// Package tui implementa o painel interativo de terminal do WatchFlow.
//
// A TUI é estritamente um cliente do Unix Domain Socket: fechá-la ou
// interrompê-la não afeta o daemon, que segue sincronizando em segundo plano.
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
)

// Panel identifica o painel em foco para navegação com Tab.
type Panel int

const (
	// PanelWatchers é a lista de cofres vigiados.
	PanelWatchers Panel = iota
	// PanelJobs é a fila persistente SQLite.
	PanelJobs
	// PanelAudit é o fluxo de eventos recentes.
	PanelAudit

	panelCount = 3
)

// Name devolve o rótulo do painel.
func (p Panel) Name() string {
	switch p {
	case PanelJobs:
		return "fila"
	case PanelAudit:
		return "auditoria"
	default:
		return "watchers"
	}
}

// DataClient abstrai as consultas ao daemon, permitindo testar o Update sem
// abrir sockets reais.
type DataClient interface {
	Status(ctx context.Context) (*ipc.StatusResponse, error)
	Jobs(ctx context.Context, watcherName string, limit int) (*ipc.JobsResponse, error)
	Sync(ctx context.Context, watcherName string) (*ipc.SyncResponse, error)
	Pause(ctx context.Context, watcherName string) (*ipc.ActionResponse, error)
	Resume(ctx context.Context, watcherName string) (*ipc.ActionResponse, error)
}

// EventSource fornece as entradas recentes de auditoria.
//
// A granularidade por step (git.add, git.commit, git.safe_sync…) vive no log
// estruturado, não no banco: pipeline_runs guarda uma linha por execução. Ler o
// log evita duplicar essa informação em uma tabela e manter duas retenções.
type EventSource interface {
	Recent(limit int) ([]logger.Entry, error)
}

// Options configura a construção do modelo.
type Options struct {
	Client       DataClient
	Events       EventSource
	Version      string
	RefreshEvery time.Duration
	JobLimit     int
	EventLimit   int
}

// Model é o estado completo da interface.
type Model struct {
	client  DataClient
	events  EventSource
	version string

	refreshEvery time.Duration
	jobLimit     int
	eventLimit   int

	status  *ipc.StatusResponse
	jobs    []ipc.JobDTO
	entries []logger.Entry

	focus    Panel
	cursor   int
	width    int
	height   int
	quitting bool

	// connErr guarda a falha de comunicação com o daemon. A TUI continua viva e
	// tentando: o daemon pode estar reiniciando via systemd.
	connErr error

	feedback     string
	feedbackKind feedbackKind
	feedbackTill time.Time
}

type feedbackKind int

const (
	feedbackInfo feedbackKind = iota
	feedbackError
)

// Mensagens assíncronas processadas pelo Update.
type (
	tickMsg   time.Time
	statusMsg struct {
		status *ipc.StatusResponse
		err    error
	}
	jobsMsg struct {
		jobs []ipc.JobDTO
		err  error
	}
	eventsMsg struct {
		entries []logger.Entry
	}
	actionMsg struct {
		message string
		err     error
	}
)

// Defaults aplicados quando Options omite valores.
const (
	defaultRefresh    = time.Second
	defaultJobLimit   = 50
	defaultEventLimit = 200
	feedbackDuration  = 4 * time.Second
)

// New constrói o modelo inicial da TUI.
func New(opts Options) Model {
	if opts.RefreshEvery <= 0 {
		opts.RefreshEvery = defaultRefresh
	}
	if opts.JobLimit <= 0 {
		opts.JobLimit = defaultJobLimit
	}
	if opts.EventLimit <= 0 {
		opts.EventLimit = defaultEventLimit
	}

	return Model{
		client:       opts.Client,
		events:       opts.Events,
		version:      opts.Version,
		refreshEvery: opts.RefreshEvery,
		jobLimit:     opts.JobLimit,
		eventLimit:   opts.EventLimit,
		focus:        PanelWatchers,
		// Dimensões provisórias até o primeiro tea.WindowSizeMsg
		width:  100,
		height: 30,
	}
}

// Init dispara a primeira carga e agenda o ciclo de atualização.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshAll(), m.scheduleTick())
}

// selectedWatcher devolve o watcher sob o cursor, se houver.
func (m Model) selectedWatcher() (ipc.WatcherStatusDTO, bool) {
	if m.status == nil || len(m.status.Watchers) == 0 {
		return ipc.WatcherStatusDTO{}, false
	}
	idx := m.cursor
	if idx < 0 || idx >= len(m.status.Watchers) {
		return ipc.WatcherStatusDTO{}, false
	}
	return m.status.Watchers[idx], true
}

// watcherCount informa quantos watchers estão listados.
func (m Model) watcherCount() int {
	if m.status == nil {
		return 0
	}
	return len(m.status.Watchers)
}
