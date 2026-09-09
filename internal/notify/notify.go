// Package notify entrega alertas ao usuário sobre eventos que exigem atenção,
// em especial o conflito de merge que interrompe um watcher (ver README, "Invariantes de engenharia").
//
// O envio é sempre best-effort: uma falha de notificação nunca interrompe nem
// altera o resultado de um pipeline.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/logger"
)

// sendTimeout limita o tempo de um envio para que um backend travado
// (notify-send sem sessão gráfica, webhook pendurado) não segure um worker.
const sendTimeout = 5 * time.Second

// repeatCooldown é a janela em que um alerta idêntico (mesmo tipo, mesmo
// watcher) é suprimido. Condições persistentes como um conflito não resolvido
// seriam reavaliadas em ciclo e transformariam um problema em uma enxurrada de
// popups críticos que o usuário teria de dispensar um a um.
const repeatCooldown = 30 * time.Minute

// Kind classifica o evento notificado e determina a urgência do alerta.
type Kind string

const (
	// KindSuccess indica sincronização concluída com sucesso.
	KindSuccess Kind = "success"
	// KindError indica falha definitiva de um pipeline.
	KindError Kind = "error"
	// KindConflict indica conflito de merge com watcher interrompido.
	KindConflict Kind = "conflict"
)

// Notification é a mensagem entregue ao usuário.
type Notification struct {
	Kind    Kind   `json:"kind"`
	Watcher string `json:"watcher"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Notifier despacha notificações para um destino concreto.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Resetter é implementado por notificadores que mantêm janela de silêncio para
// alertas repetidos e podem tê-la limpa para um watcher específico.
type Resetter interface {
	Reset(watcher string)
}

// Options espelha a seção 'notifications' da configuração. O pacote define seu
// próprio tipo em vez de importar internal/config, que precisa validar webhooks
// através daqui — importar de volta fecharia um ciclo.
type Options struct {
	Enabled    bool
	OnSuccess  bool
	OnError    bool
	OnConflict bool
	Backend    string
	WebhookURL string
}

// New constrói o notificador a partir da seção 'notifications' da configuração.
// Nunca retorna erro fatal por indisponibilidade do backend: se o destino
// preferido não estiver acessível (ex.: notify-send ausente), recai para o log
// e avisa, em vez de deixar o usuário sem qualquer alerta.
func New(cfg Options) Notifier {
	log := logger.For("notify")

	if !cfg.Enabled {
		return newDispatcher(cfg, noopNotifier{}, log)
	}

	var backend Notifier

	switch cfg.Backend {
	case "desktop":
		d, err := newDesktopNotifier()
		if err != nil {
			log.Warn("backend 'desktop' indisponível; alertas seguirão apenas para o log",
				slog.Any("error", err))
			backend = newLogNotifier()
		} else {
			backend = d
		}

	case "webhook":
		w, err := newWebhookNotifier(cfg.WebhookURL)
		if err != nil {
			log.Warn("backend 'webhook' indisponível; alertas seguirão apenas para o log",
				slog.Any("error", err))
			backend = newLogNotifier()
		} else {
			backend = w
		}

	default:
		backend = newLogNotifier()
	}

	return newDispatcher(cfg, backend, log)
}

func newDispatcher(cfg Options, backend Notifier, log *slog.Logger) *dispatcher {
	return &dispatcher{
		cfg:      cfg,
		backend:  backend,
		log:      log,
		cooldown: repeatCooldown,
		lastSent: make(map[string]time.Time),
	}
}

// dispatcher aplica os filtros on_success/on_error/on_conflict antes de delegar.
type dispatcher struct {
	cfg     Options
	backend Notifier
	log     *slog.Logger

	cooldown time.Duration
	mu       sync.Mutex
	lastSent map[string]time.Time
}

func (d *dispatcher) Notify(ctx context.Context, n Notification) error {
	if !d.enabledFor(n.Kind) {
		return nil
	}

	if d.suppressedAsRepeat(n) {
		d.log.Debug("alerta repetido suprimido",
			slog.String("kind", string(n.Kind)), slog.String("watcher", n.Watcher))
		return nil
	}

	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()

	if err := d.backend.Notify(sendCtx, n); err != nil {
		d.log.Warn("falha ao entregar notificação",
			slog.String("kind", string(n.Kind)),
			slog.String("watcher", n.Watcher),
			slog.Any("error", err))
		return err
	}

	return nil
}

// suppressedAsRepeat aplica a janela de silêncio a alertas de condição
// persistente. Sucessos não são deduplicados: cada sincronização é um evento
// distinto e relevante para quem optou por recebê-los.
func (d *dispatcher) suppressedAsRepeat(n Notification) bool {
	if n.Kind == KindSuccess {
		return false
	}

	key := string(n.Kind) + "|" + n.Watcher
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()

	if last, seen := d.lastSent[key]; seen && now.Sub(last) < d.cooldown {
		return true
	}

	d.lastSent[key] = now
	return false
}

// Reset limpa o histórico de supressão de um watcher, permitindo que um novo
// alerta seja emitido imediatamente. Chamado quando o usuário retoma um watcher.
func (d *dispatcher) Reset(watcher string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, kind := range []Kind{KindConflict, KindError} {
		delete(d.lastSent, string(kind)+"|"+watcher)
	}
}

func (d *dispatcher) enabledFor(kind Kind) bool {
	if !d.cfg.Enabled {
		return false
	}
	switch kind {
	case KindSuccess:
		return d.cfg.OnSuccess
	case KindError:
		return d.cfg.OnError
	case KindConflict:
		return d.cfg.OnConflict
	default:
		return false
	}
}

// noopNotifier descarta tudo; usado quando as notificações estão desabilitadas.
type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, Notification) error { return nil }

// logNotifier registra o alerta no log estruturado. É também o fallback quando
// o backend configurado não está disponível.
type logNotifier struct {
	log *slog.Logger
}

func newLogNotifier() *logNotifier {
	return &logNotifier{log: logger.For("notify")}
}

func (l *logNotifier) Notify(_ context.Context, n Notification) error {
	attrs := []any{
		slog.String("kind", string(n.Kind)),
		slog.String("watcher", n.Watcher),
		slog.String("mensagem", n.Message),
	}
	if n.Detail != "" {
		attrs = append(attrs, slog.String("detalhe", n.Detail))
	}

	switch n.Kind {
	case KindConflict, KindError:
		l.log.Error(n.Title, attrs...)
	default:
		l.log.Info(n.Title, attrs...)
	}
	return nil
}

// Conflict monta o alerta obrigatório de conflito de merge (ver README, "Invariantes de engenharia").
func Conflict(watcher, detail string) Notification {
	return Notification{
		Kind:    KindConflict,
		Watcher: watcher,
		Title:   fmt.Sprintf("WatchFlow: conflito em '%s'", watcher),
		Message: fmt.Sprintf(
			"Conflito de merge detectado. A árvore de trabalho foi restaurada intacta e a sincronização de '%s' está interrompida. "+
				"Resolva manualmente e execute 'watchflow resume %s'.", watcher, watcher),
		Detail: detail,
	}
}

// Failure monta o alerta de falha definitiva de um pipeline.
func Failure(watcher, pipeline, detail string) Notification {
	return Notification{
		Kind:    KindError,
		Watcher: watcher,
		Title:   fmt.Sprintf("WatchFlow: falha em '%s'", watcher),
		Message: fmt.Sprintf("O pipeline '%s' falhou e não será retentado. Verifique 'watchflow status'.", pipeline),
		Detail:  detail,
	}
}

// Success monta o alerta de sincronização concluída (silencioso por padrão).
func Success(watcher string, files int, duration time.Duration) Notification {
	return Notification{
		Kind:    KindSuccess,
		Watcher: watcher,
		Title:   fmt.Sprintf("WatchFlow: '%s' sincronizado", watcher),
		Message: fmt.Sprintf("%d arquivo(s) sincronizados em %s.", files, duration.Truncate(time.Millisecond)),
	}
}
