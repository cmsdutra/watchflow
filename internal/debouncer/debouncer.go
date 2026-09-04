package debouncer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/normalizer"
)

// BatchTrigger indica o motivo pelo qual o lote de eventos foi despachado.
type BatchTrigger string

const (
	// TriggerSilence indica despacho por silêncio da janela deslizante (trailing debounce).
	TriggerSilence BatchTrigger = "DEBOUNCE_SILENCE"
	// TriggerMaxWait indica despacho forçado por alcance do teto rígido (hard deadline).
	TriggerMaxWait BatchTrigger = "MAX_WAIT_DEADLINE"
	// TriggerFlush indica despacho forçado pelo encerramento gracioso do serviço.
	TriggerFlush BatchTrigger = "FLUSH_ON_STOP"
)

// EventBatch representa um lote coalescido e deduplicado de arquivos alterados.
type EventBatch struct {
	WatcherName string       `json:"watcher_name"`
	Files       []string     `json:"files"`
	EventsCount int          `json:"events_count"`
	FirstEvent  time.Time    `json:"first_event"`
	LastEvent   time.Time    `json:"last_event"`
	Trigger     BatchTrigger `json:"trigger"`
}

// Debouncer agrega rajadas rápidas de eventos de escrita usando uma janela deslizante
// limitada por um teto absoluto (max_wait).
type Debouncer struct {
	watcherName string
	debounce    time.Duration
	maxWait     time.Duration

	in   chan normalizer.NormalizedEvent
	out  chan EventBatch
	done chan struct{}

	mu            sync.Mutex
	pendingFiles  map[string]struct{}
	eventsCount   int
	firstEventAt  time.Time
	lastEventAt   time.Time
	debounceTimer *time.Timer
	maxWaitTimer  *time.Timer
	timerActive   bool
	closed        bool
}

// New instancia um novo Debouncer validando os parâmetros temporais.
func New(watcherName string, debounce, maxWait time.Duration) (*Debouncer, error) {
	if watcherName == "" {
		return nil, fmt.Errorf("nome do watcher não pode ser vazio")
	}
	if debounce <= 0 {
		return nil, fmt.Errorf("debounce deve ser maior que zero (recebido: %v)", debounce)
	}
	if maxWait < debounce {
		return nil, fmt.Errorf("max_wait (%v) não pode ser menor que debounce (%v)", maxWait, debounce)
	}

	return &Debouncer{
		watcherName:  watcherName,
		debounce:     debounce,
		maxWait:      maxWait,
		in:           make(chan normalizer.NormalizedEvent, 1024),
		out:          make(chan EventBatch, 64),
		done:         make(chan struct{}),
		pendingFiles: make(map[string]struct{}),
	}, nil
}

// In retorna o canal de entrada para recepção de eventos normalizados.
func (d *Debouncer) In() chan<- normalizer.NormalizedEvent {
	return d.in
}

// Out retorna o canal de saída de lotes coalescidos.
func (d *Debouncer) Out() <-chan EventBatch {
	return d.out
}

// Add insere um evento normalizado diretamente no buffer do debouncer.
func (d *Debouncer) Add(ev normalizer.NormalizedEvent) bool {
	select {
	case d.in <- ev:
		return true
	default:
		return false
	}
}

// Start inicia o loop assíncrono de debounce e timers.
func (d *Debouncer) Start(ctx context.Context) {
	go d.loop(ctx)
}

func (d *Debouncer) loop(ctx context.Context) {
	d.debounceTimer = time.NewTimer(d.debounce)
	stopTimer(d.debounceTimer)

	d.maxWaitTimer = time.NewTimer(d.maxWait)
	stopTimer(d.maxWaitTimer)

	for {
		select {
		case <-ctx.Done():
			d.flush(TriggerFlush)
			return

		case <-d.done:
			d.flush(TriggerFlush)
			return

		case ev, ok := <-d.in:
			if !ok {
				d.flush(TriggerFlush)
				return
			}
			d.handleEvent(ev)

		case <-d.debounceTimer.C:
			d.handleTimeout(TriggerSilence)

		case <-d.maxWaitTimer.C:
			d.handleTimeout(TriggerMaxWait)
		}
	}
}

func (d *Debouncer) handleEvent(ev normalizer.NormalizedEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := ev.Timestamp
	if now.IsZero() {
		now = time.Now()
	}

	targetPath := ev.RelPath
	if targetPath == "" {
		targetPath = ev.AbsPath
	}

	d.pendingFiles[targetPath] = struct{}{}
	d.eventsCount++
	d.lastEventAt = now

	if !d.timerActive {
		// Primeiro evento do lote: inicia ambos os timers
		d.firstEventAt = now
		d.timerActive = true
		d.debounceTimer.Reset(d.debounce)
		d.maxWaitTimer.Reset(d.maxWait)
	} else {
		// Evento subsequente: rearranja apenas a janela deslizante
		stopTimer(d.debounceTimer)
		d.debounceTimer.Reset(d.debounce)
	}
}

func (d *Debouncer) handleTimeout(trigger BatchTrigger) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.timerActive || len(d.pendingFiles) == 0 {
		return
	}

	stopTimer(d.debounceTimer)
	stopTimer(d.maxWaitTimer)
	d.timerActive = false

	batch := d.buildBatchLocked(trigger)
	d.resetBatchLocked()

	select {
	case d.out <- batch:
	default:
		// Canal de saída cheio
	}
}

func (d *Debouncer) flush(trigger BatchTrigger) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.timerActive || len(d.pendingFiles) == 0 {
		return
	}

	stopTimer(d.debounceTimer)
	stopTimer(d.maxWaitTimer)
	d.timerActive = false

	batch := d.buildBatchLocked(trigger)
	d.resetBatchLocked()

	select {
	case d.out <- batch:
	default:
	}
}

func (d *Debouncer) buildBatchLocked(trigger BatchTrigger) EventBatch {
	files := make([]string, 0, len(d.pendingFiles))
	for f := range d.pendingFiles {
		files = append(files, f)
	}
	sort.Strings(files)

	return EventBatch{
		WatcherName: d.watcherName,
		Files:       files,
		EventsCount: d.eventsCount,
		FirstEvent:  d.firstEventAt,
		LastEvent:   d.lastEventAt,
		Trigger:     trigger,
	}
}

func (d *Debouncer) resetBatchLocked() {
	d.pendingFiles = make(map[string]struct{})
	d.eventsCount = 0
	d.firstEventAt = time.Time{}
	d.lastEventAt = time.Time{}
}

// Stop encerra o debouncer descarregando qualquer lote pendente.
func (d *Debouncer) Stop() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.done)
	d.mu.Unlock()
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
