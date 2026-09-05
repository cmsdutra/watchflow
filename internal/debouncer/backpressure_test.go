package debouncer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/watcher"
)

func makeEvent(name, relPath string) normalizer.NormalizedEvent {
	return normalizer.NormalizedEvent{
		WatcherName: name,
		RelPath:     relPath,
		Op:          watcher.OpWrite,
		Timestamp:   time.Now(),
	}
}

// TestNoBatchDroppedWhenConsumerIsSlow cobre a regressão do descarte silencioso
// de lotes: com o canal de saída saturado, a implementação antiga usava
// 'default:' e perdia permanentemente as alterações do usuário.
func TestNoBatchDroppedWhenConsumerIsSlow(t *testing.T) {
	d, err := New("vault-slow", 20*time.Millisecond, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)

	// Satura o canal de saída (buffer 64) sem consumir nada.
	const totalBatches = 80
	for i := 0; i < totalBatches; i++ {
		d.in <- makeEvent("vault-slow", fmt.Sprintf("arquivo_%d.md", i))
		// Espera o disparo por silêncio para forçar um lote por arquivo
		time.Sleep(35 * time.Millisecond)
	}

	// Drena tudo e confere que nenhum arquivo se perdeu no caminho
	seen := make(map[string]bool)
	deadline := time.After(15 * time.Second)

drain:
	for len(seen) < totalBatches {
		select {
		case batch, ok := <-d.Out():
			if !ok {
				break drain
			}
			for _, f := range batch.Files {
				seen[f] = true
			}
		case <-deadline:
			break drain
		}
	}

	if len(seen) != totalBatches {
		t.Fatalf("esperava %d arquivos distintos entregues, obteve %d (lotes descartados silenciosamente)", totalBatches, len(seen))
	}
}

// TestEmitRequeuesInsteadOfDropping garante que, esgotado o prazo de
// contrapressão, o lote volta para o buffer pendente em vez de sumir.
func TestEmitRequeuesInsteadOfDropping(t *testing.T) {
	d, err := New("vault-requeue", 20*time.Millisecond, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}
	// Prazo curto para exercitar o caminho de devolução ao buffer
	d.emitTimeout = 50 * time.Millisecond

	// Preenche o canal de saída manualmente, sem iniciar o loop
	for i := 0; i < cap(d.out); i++ {
		d.out <- EventBatch{WatcherName: "filler"}
	}

	batch := EventBatch{
		WatcherName: "vault-requeue",
		Files:       []string{"perdido.md", "tambem_perdido.md"},
		EventsCount: 2,
		FirstEvent:  time.Now(),
		LastEvent:   time.Now(),
	}

	// Timers precisam existir para o requeue rearmá-los
	d.debounceTimer = time.NewTimer(d.debounce)
	stopTimer(d.debounceTimer)
	d.maxWaitTimer = time.NewTimer(d.maxWait)
	stopTimer(d.maxWaitTimer)

	d.emit(batch)

	d.mu.Lock()
	pending := len(d.pendingFiles)
	active := d.timerActive
	d.mu.Unlock()

	if pending != 2 {
		t.Fatalf("esperava os 2 arquivos devolvidos ao buffer pendente, obteve %d", pending)
	}
	if !active {
		t.Error("esperava os timers rearmados para reemissão do lote devolvido")
	}
}

// TestFinalFlushIsDeliveredAndChannelClosed garante que o lote acumulado no
// momento do encerramento é entregue e que o consumidor recebe o fechamento do
// canal como sinal de fim de fluxo.
func TestFinalFlushIsDeliveredAndChannelClosed(t *testing.T) {
	d, err := New("vault-flush", 5*time.Second, 10*time.Second)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	d.in <- makeEvent("vault-flush", "nao_pode_sumir.md")
	time.Sleep(100 * time.Millisecond)

	// Encerra antes do debounce de 5s expirar
	cancel()

	var delivered []string
	closed := false
	timeout := time.After(3 * time.Second)

	for !closed {
		select {
		case batch, ok := <-d.Out():
			if !ok {
				closed = true
				break
			}
			if batch.Trigger != TriggerFlush {
				t.Errorf("esperava gatilho %s, obteve %s", TriggerFlush, batch.Trigger)
			}
			delivered = append(delivered, batch.Files...)
		case <-timeout:
			t.Fatal("timeout: canal de saída não foi fechado após o encerramento")
		}
	}

	if len(delivered) != 1 || delivered[0] != "nao_pode_sumir.md" {
		t.Fatalf("esperava o lote final entregue com 'nao_pode_sumir.md', obteve %v", delivered)
	}
}
