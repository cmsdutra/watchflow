package pipeline

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/providers"
	"github.com/watchflow/watchflow/internal/queue"
)

// ErrorCategory categoriza a severidade e tratativa de um erro de pipeline.
type ErrorCategory int

const (
	// CategoryTransient indica falhas recuperáveis (rede, timeout, lock temporário).
	CategoryTransient ErrorCategory = iota
	// CategoryConflict indica colisão de alterações (merge conflict) que exige interrupção.
	CategoryConflict
	// CategoryFatal indica erros irrecuperáveis de configuração, sintaxe ou permissão.
	CategoryFatal
)

// String retorna a representação textual da categoria do erro.
func (c ErrorCategory) String() string {
	switch c {
	case CategoryTransient:
		return "TRANSIENT"
	case CategoryConflict:
		return "CONFLICT"
	case CategoryFatal:
		return "FATAL"
	default:
		return "UNKNOWN"
	}
}

// JobContext encapsula o estado e metadados durante a execução sequencial de um pipeline.
type JobContext struct {
	Context     context.Context
	Cancel      context.CancelFunc
	Job         *queue.Job
	Watcher     *config.WatcherConfig
	Pipeline    *config.Pipeline
	StepResults map[string]*providers.StepResult
	LastOutput  string
	StartTime   time.Time
	mu          sync.RWMutex
}

// NewJobContext inicializa uma nova sessão de execução de job.
func NewJobContext(
	ctx context.Context,
	cancel context.CancelFunc,
	job *queue.Job,
	watcher *config.WatcherConfig,
	pipe *config.Pipeline,
) *JobContext {
	return &JobContext{
		Context:     ctx,
		Cancel:      cancel,
		Job:         job,
		Watcher:     watcher,
		Pipeline:    pipe,
		StepResults: make(map[string]*providers.StepResult),
		StartTime:   time.Now(),
	}
}

// RecordStep registra com segurança de concorrência o resultado de um step.
func (jc *JobContext) RecordStep(action string, res *providers.StepResult) {
	jc.mu.Lock()
	defer jc.mu.Unlock()

	jc.StepResults[action] = res
	if res != nil && res.Output != "" {
		jc.LastOutput = res.Output
	}
}

// GetLastOutput retorna com thread-safety a saída do último step executado.
func (jc *JobContext) GetLastOutput() string {
	jc.mu.RLock()
	defer jc.mu.RUnlock()
	return jc.LastOutput
}

// Duration retorna o tempo decorrido desde o início da execução do job.
func (jc *JobContext) Duration() time.Duration {
	return time.Since(jc.StartTime)
}

// ClassifyError analisa o erro retornado e determina sua categoria conforme AGENTS.md.
func ClassifyError(err error, res *providers.StepResult) ErrorCategory {
	if err == nil && (res == nil || res.Success) {
		return CategoryTransient
	}

	// 1. Conflito de Merge
	if (res != nil && res.ConflictErr) || errors.Is(err, providers.ErrConflict) {
		return CategoryConflict
	}
	if err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "merge conflict") ||
			strings.Contains(errLower, "conflict halted") ||
			strings.Contains(errLower, "automatic merge failed") {
			return CategoryConflict
		}
	}

	// 2. Erros Transitórios (Timeout, Rede, Locks)
	if res != nil && res.TransientErr {
		return CategoryTransient
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CategoryTransient
	}

	if err != nil {
		errLower := strings.ToLower(err.Error())
		transientIndicators := []string{
			"index.lock",
			"connection refused",
			"network is unreachable",
			"i/o timeout",
			"operation timed out",
			"temporary failure in name resolution",
			"could not resolve host",
			"could not connect to",
			"failed to connect to",
			"unable to access",
			"resource temporarily unavailable",
			"the remote end hung up unexpectedly",
			"tls handshake",
			"ssl_error",
			"non-fast-forward",
			"[rejected]",
			"fetch first",
		}
		for _, indicator := range transientIndicators {
			if strings.Contains(errLower, indicator) {
				return CategoryTransient
			}
		}
	}

	// 3. Demais falhas são tratadas como fatais (parada definitiva)
	return CategoryFatal
}

// CalculateBackoff calcula o tempo de espera com backoff exponencial e jitter:
// T_wait = min(300s, 5s * 2^retry + rand(0, 3s)).
func CalculateBackoff(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	// Limitar retryCount a 6 para evitar overflow em cálculos de 2^retry
	if retryCount > 6 {
		retryCount = 6
	}

	baseSeconds := 5 * (1 << retryCount)
	jitterMs := rand.IntN(3000) // 0 a 2999 ms

	backoff := (time.Duration(baseSeconds) * time.Second) + (time.Duration(jitterMs) * time.Millisecond)
	maxBackoff := 300 * time.Second

	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}
