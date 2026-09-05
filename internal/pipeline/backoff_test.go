package pipeline_test

import (
	"errors"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
)

func TestClassifyBackoffSeparatesContentionFromOutage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		res  *providers.StepResult
		want pipeline.BackoffProfile
	}{
		{
			"push rejeitado pelo avanço do remoto",
			errors.New("git push origin main: ! [rejected] main -> main (fetch first)"),
			nil,
			pipeline.BackoffContention,
		},
		{
			"non-fast-forward",
			errors.New("Updates were rejected because the tip of your current branch is behind"),
			nil,
			pipeline.BackoffContention,
		},
		{
			"detectado via StepResult",
			errors.New("falha"),
			&providers.StepResult{ErrorMessage: "git push falhou: non-fast-forward"},
			pipeline.BackoffContention,
		},
		{
			"rede indisponível",
			errors.New("could not resolve host: github.com"),
			nil,
			pipeline.BackoffNetwork,
		},
		{
			"trava externa",
			errors.New("index.lock ainda ativa"),
			nil,
			pipeline.BackoffNetwork,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pipeline.ClassifyBackoff(tc.err, tc.res); got != tc.want {
				t.Errorf("perfil = %s, esperado %s", got, tc.want)
			}
		})
	}
}

// TestContentionConvergesFasterThanOutage fixa o comportamento pedido: duas
// máquinas ativas disputando o mesmo remoto precisam reconvergir em segundos,
// não esperar o backoff exponencial de indisponibilidade (que chega a 300s).
func TestContentionConvergesFasterThanOutage(t *testing.T) {
	for retry := 0; retry < 6; retry++ {
		contention := pipeline.BackoffFor(pipeline.BackoffContention, retry)
		network := pipeline.BackoffFor(pipeline.BackoffNetwork, retry)

		if contention >= network {
			t.Errorf("retry %d: espera por contenção (%v) deveria ser menor que a de rede (%v)",
				retry, contention, network)
		}
		if contention > 15*time.Second {
			t.Errorf("retry %d: espera por contenção estourou o teto: %v", retry, contention)
		}
	}

	// O tempo total até esgotar 5 tentativas precisa ser da ordem de segundos
	var total time.Duration
	for retry := 0; retry < 5; retry++ {
		total += pipeline.BackoffFor(pipeline.BackoffContention, retry)
	}
	if total > 45*time.Second {
		t.Errorf("convergência sob disputa demoraria %v no total; esperado bem abaixo disso", total)
	}
}

func TestNetworkBackoffUnchanged(t *testing.T) {
	// O perfil de rede preserva o comportamento original documentado:
	// min(300s, 5s * 2^retry + jitter)
	for retry := 0; retry < 8; retry++ {
		got := pipeline.BackoffFor(pipeline.BackoffNetwork, retry)
		want := pipeline.CalculateBackoff(retry)

		// Ambos têm jitter aleatório; compara a ordem de grandeza
		if got < want-3*time.Second || got > want+3*time.Second {
			t.Errorf("retry %d: perfil de rede (%v) divergiu de CalculateBackoff (%v)", retry, got, want)
		}
		if got > 300*time.Second {
			t.Errorf("retry %d: excedeu o teto de 300s: %v", retry, got)
		}
	}
}
