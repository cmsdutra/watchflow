package debouncer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/watcher"
)

func TestDebounceValidation(t *testing.T) {
	// 1. Nome vazio
	_, err := New("", 10*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Errorf("esperava erro para nome de watcher vazio")
	}

	// 2. Debounce <= 0
	_, err = New("w", 0, 50*time.Millisecond)
	if err == nil {
		t.Errorf("esperava erro para debounce <= 0")
	}

	// 3. MaxWait < Debounce
	_, err = New("w", 50*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Errorf("esperava erro para maxWait < debounce")
	}

	// 4. Parâmetros válidos
	d, err := New("w", 20*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("esperava sucesso, obteve: %v", err)
	}
	if d == nil {
		t.Fatal("debouncer não deveria ser nulo")
	}
}

func TestTrailingDebounceBurst(t *testing.T) {
	debounce := 50 * time.Millisecond
	maxWait := 300 * time.Millisecond

	d, err := New("vault-burst", debounce, maxWait)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)

	// Simula rajada rápida de 50 eventos modificando 5 arquivos distintos
	for i := 0; i < 50; i++ {
		fileIdx := i % 5
		ev := normalizer.NormalizedEvent{
			WatcherName: "vault-burst",
			RelPath:     fmt.Sprintf("arquivo_%d.md", fileIdx),
			Op:          watcher.OpWrite,
			Timestamp:   time.Now(),
		}
		if ok := d.Add(ev); !ok {
			t.Fatalf("falha ao adicionar evento %d", i)
		}
		time.Sleep(1 * time.Millisecond)
	}

	// Aguarda o disparo por silêncio
	select {
	case batch := <-d.Out():
		if batch.WatcherName != "vault-burst" {
			t.Errorf("esperava watcher 'vault-burst', obteve: %s", batch.WatcherName)
		}
		if batch.Trigger != TriggerSilence {
			t.Errorf("esperava gatilho TriggerSilence, obteve: %s", batch.Trigger)
		}
		if batch.EventsCount != 50 {
			t.Errorf("esperava EventsCount 50, obteve: %d", batch.EventsCount)
		}
		// Apenas 5 arquivos únicos
		if len(batch.Files) != 5 {
			t.Errorf("esperava 5 arquivos únicos coalescidos, obteve %d: %v", len(batch.Files), batch.Files)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("timeout aguardando lote coalescido por silêncio")
	}

	// Garante que nenhum segundo lote fantasma foi emitido
	select {
	case unexpected := <-d.Out():
		t.Fatalf("lote inesperado recebido: %v", unexpected)
	case <-time.After(100 * time.Millisecond):
		// Sucesso
	}
}

func TestMaxWaitForcedDispatch(t *testing.T) {
	debounce := 60 * time.Millisecond
	maxWait := 150 * time.Millisecond

	d, err := New("vault-continuous", debounce, maxWait)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)

	stopPumping := make(chan struct{})
	defer close(stopPumping)

	// Injeta eventos continuamente a cada 25ms (menor que o debounce de 60ms)
	// Isso impede que o debounce expire por silêncio, forçando o maxWait!
	go func() {
		counter := 0
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopPumping:
				return
			case <-ticker.C:
				counter++
				d.Add(normalizer.NormalizedEvent{
					WatcherName: "vault-continuous",
					RelPath:     fmt.Sprintf("nota_%d.md", counter),
					Op:          watcher.OpWrite,
					Timestamp:   time.Now(),
				})
			}
		}
	}()

	select {
	case batch := <-d.Out():
		if batch.Trigger != TriggerMaxWait {
			t.Errorf("esperava gatilho TriggerMaxWait por alcance do teto rígido, obteve: %s", batch.Trigger)
		}
		if batch.EventsCount < 4 {
			t.Errorf("esperava pelo menos 4 eventos agregados antes do teto, obteve: %d", batch.EventsCount)
		}

	case <-time.After(1 * time.Second):
		t.Fatal("timeout aguardando disparo forçado pelo teto max_wait")
	}
}

func TestFlushOnContextDone(t *testing.T) {
	debounce := 500 * time.Millisecond
	maxWait := 1 * time.Second

	d, err := New("vault-flush", debounce, maxWait)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)

	// Adiciona evento
	d.Add(normalizer.NormalizedEvent{
		WatcherName: "vault-flush",
		RelPath:     "urgente.md",
		Op:          watcher.OpWrite,
		Timestamp:   time.Now(),
	})

	// Cancela o contexto antes que qualquer timer expire
	time.Sleep(20 * time.Millisecond)
	cancel()

	// Deve descarregar o lote pendente com TriggerFlush
	select {
	case batch := <-d.Out():
		if batch.Trigger != TriggerFlush {
			t.Errorf("esperava gatilho TriggerFlush no cancelamento, obteve: %s", batch.Trigger)
		}
		if len(batch.Files) != 1 || batch.Files[0] != "urgente.md" {
			t.Errorf("arquivo esperado 'urgente.md' não encontrado no lote: %v", batch.Files)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout aguardando flush no cancelamento do contexto")
	}
}

func TestSequentialBatches(t *testing.T) {
	debounce := 30 * time.Millisecond
	maxWait := 100 * time.Millisecond

	d, err := New("vault-seq", debounce, maxWait)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)

	// Lote 1
	d.Add(normalizer.NormalizedEvent{WatcherName: "vault-seq", RelPath: "lote1.md", Op: watcher.OpWrite})
	select {
	case b1 := <-d.Out():
		if len(b1.Files) != 1 || b1.Files[0] != "lote1.md" {
			t.Errorf("lote 1 incorreto: %v", b1.Files)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout no lote 1")
	}

	// Lote 2 após o primeiro ter sido concluído
	d.Add(normalizer.NormalizedEvent{WatcherName: "vault-seq", RelPath: "lote2.md", Op: watcher.OpWrite})
	select {
	case b2 := <-d.Out():
		if len(b2.Files) != 1 || b2.Files[0] != "lote2.md" {
			t.Errorf("lote 2 incorreto: %v", b2.Files)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout no lote 2")
	}
}

func TestDebouncerStopIdempotent(t *testing.T) {
	d, err := New("vault-stop", 50*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("falha ao criar debouncer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)

	// Chama Stop múltiplas vezes
	d.Stop()
	d.Stop()
}

func TestDebouncerInChannel(t *testing.T) {
	d, err := New("w", 50*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if d.In() == nil {
		t.Errorf("In() não deveria ser nulo")
	}
}
