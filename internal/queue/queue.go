package queue

import (
	"time"
)

// JobStatus representa os estados de ciclo de vida de um Job na fila persistente.
type JobStatus string

const (
	// StatusPending indica que o job está aguardando execução.
	StatusPending JobStatus = "PENDING"
	// StatusRunning indica que o job foi alocado por um worker e está em processamento.
	StatusRunning JobStatus = "RUNNING"
	// StatusCompleted indica que o job foi executado e concluído com sucesso.
	StatusCompleted JobStatus = "COMPLETED"
	// StatusFailed indica que o job falhou definitivamente após esgotar tentativas.
	StatusFailed JobStatus = "FAILED"
	// StatusBlocked indica que o job foi bloqueado por conflito insolúvel (merge conflict).
	StatusBlocked JobStatus = "BLOCKED"
)

// WatcherStatus reflete o estado de integridade operacional de um diretório vigiado.
type WatcherStatus string

const (
	// WatcherHealthy indica operação regular e sincronização bem-sucedida.
	WatcherHealthy WatcherStatus = "HEALTHY"
	// WatcherDegraded indica falhas temporárias (rede offline, retries em andamento).
	WatcherDegraded WatcherStatus = "DEGRADED"
	// WatcherPaused indica que a captura e os pipelines foram pausados pelo usuário.
	WatcherPaused WatcherStatus = "PAUSED"
	// WatcherConflictHalted indica interrupção obrigatória por conflito de merge Git.
	WatcherConflictHalted WatcherStatus = "CONFLICT_HALTED"
)

// Job representa uma unidade persistida de trabalho a ser executada pelo Pipeline Engine.
type Job struct {
	ID           string     `json:"id"`
	WatcherID    string     `json:"watcher_id"`
	PipelineName string     `json:"pipeline_name"`
	Status       JobStatus  `json:"status"`
	PayloadFiles []string   `json:"payload_files"`
	RetryCount   int        `json:"retry_count"`
	MaxRetries   int        `json:"max_retries"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	LockedBy     string     `json:"locked_by,omitempty"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// WatcherRecord armazena os metadados persistentes e métricas de saúde de cada watcher.
type WatcherRecord struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Path              string        `json:"path"`
	Status            WatcherStatus `json:"status"`
	LastEventAt       *time.Time    `json:"last_event_at,omitempty"`
	LastSuccessSyncAt *time.Time    `json:"last_success_sync_at,omitempty"`
	LastFailedSyncAt  *time.Time    `json:"last_failed_sync_at,omitempty"`
	LastErrorMessage  string        `json:"last_error_message,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

// PipelineRun registra o histórico de auditoria de cada execução de pipeline.
type PipelineRun struct {
	ID           string    `json:"id"`
	JobID        string    `json:"job_id,omitempty"`
	WatcherID    string    `json:"watcher_id"`
	PipelineName string    `json:"pipeline_name"`
	Status       string    `json:"status"`
	DurationMs   int64     `json:"duration_ms"`
	ErrorStep    string    `json:"error_step,omitempty"`
	ErrorDetails string    `json:"error_details,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}
