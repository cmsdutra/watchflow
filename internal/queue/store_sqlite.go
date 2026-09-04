package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Import do driver pure-Go sqlite para registro no database/sql
	_ "modernc.org/sqlite"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS watchers (
    id TEXT PRIMARY KEY,
    name TEXT UNIQUE NOT NULL,
    path TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('HEALTHY', 'DEGRADED', 'PAUSED', 'CONFLICT_HALTED')),
    last_event_at DATETIME,
    last_success_sync_at DATETIME,
    last_failed_sync_at DATETIME,
    last_error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    watcher_id TEXT NOT NULL REFERENCES watchers(id) ON DELETE CASCADE,
    pipeline_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'BLOCKED')),
    payload_files JSON NOT NULL,
    retry_count INTEGER DEFAULT 0,
    max_retries INTEGER DEFAULT 5,
    scheduled_for DATETIME DEFAULT CURRENT_TIMESTAMP,
    locked_by TEXT,
    locked_at DATETIME,
    last_error TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS pipeline_runs (
    id TEXT PRIMARY KEY,
    job_id TEXT REFERENCES jobs(id),
    watcher_id TEXT NOT NULL,
    pipeline_name TEXT NOT NULL,
    status TEXT NOT NULL,
    duration_ms INTEGER,
    error_step TEXT,
    error_details TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS resource_locks (
    resource_path TEXT PRIMARY KEY,
    owner_pid INTEGER NOT NULL,
    acquired_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    heartbeat_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_jobs_pending ON jobs(status, scheduled_for);
CREATE INDEX IF NOT EXISTS idx_runs_created ON pipeline_runs(created_at);
`

// Store gerencia o acesso transacional à base de dados SQLite do WatchFlow.
type Store struct {
	db *sql.DB
}

// NewSQLiteStore inicializa o banco SQLite, configura pragmas de concorrência WAL e aplica o schema.
func NewSQLiteStore(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("falha ao criar diretório do banco de dados '%s': %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("falha ao abrir banco de dados SQLite '%s': %w", dbPath, err)
	}

	// Pragmas para garantir integridade, concorrência e WAL mode
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
	}

	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("falha ao aplicar pragma '%s': %w", p, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("falha ao inicializar schema do banco: %w", err)
	}

	return &Store{db: db}, nil
}

// Close encerra a conexão com o banco SQLite.
func (s *Store) Close() error {
	return s.db.Close()
}

// RegisterWatcher cadastra ou atualiza o registro de um watcher vigiado.
func (s *Store) RegisterWatcher(w *WatcherRecord) error {
	query := `
	INSERT INTO watchers (id, name, path, status, created_at, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		path = excluded.path,
		status = excluded.status,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query, w.ID, w.Name, w.Path, w.Status)
	if err != nil {
		return fmt.Errorf("falha ao registrar watcher '%s': %w", w.Name, err)
	}
	return nil
}

// GetWatcher busca um watcher pelo ID.
func (s *Store) GetWatcher(id string) (*WatcherRecord, error) {
	query := `
	SELECT id, name, path, status, last_event_at, last_success_sync_at, last_failed_sync_at, COALESCE(last_error_message, ''), created_at, updated_at
	FROM watchers WHERE id = ?;
	`
	row := s.db.QueryRow(query, id)
	return scanWatcher(row)
}

// GetWatcherByName busca um watcher pelo nome configurado.
func (s *Store) GetWatcherByName(name string) (*WatcherRecord, error) {
	query := `
	SELECT id, name, path, status, last_event_at, last_success_sync_at, last_failed_sync_at, COALESCE(last_error_message, ''), created_at, updated_at
	FROM watchers WHERE name = ?;
	`
	row := s.db.QueryRow(query, name)
	return scanWatcher(row)
}

// ListWatchers retorna todos os watchers cadastrados.
func (s *Store) ListWatchers() ([]*WatcherRecord, error) {
	query := `
	SELECT id, name, path, status, last_event_at, last_success_sync_at, last_failed_sync_at, COALESCE(last_error_message, ''), created_at, updated_at
	FROM watchers ORDER BY name ASC;
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar watchers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var watchers []*WatcherRecord
	for rows.Next() {
		w, err := scanWatcherRow(rows)
		if err != nil {
			return nil, err
		}
		watchers = append(watchers, w)
	}
	return watchers, rows.Err()
}

// UpdateWatcherStatus altera o status de saúde e a última mensagem de erro de um watcher.
func (s *Store) UpdateWatcherStatus(id string, status WatcherStatus, lastErr string) error {
	query := `
	UPDATE watchers
	SET status = ?, last_error_message = ?, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, status, lastErr, id)
	if err != nil {
		return fmt.Errorf("falha ao atualizar status do watcher '%s': %w", id, err)
	}
	return nil
}

// UpdateWatcherSyncTime atualiza o timestamp de sucesso ou falha da última sincronização.
func (s *Store) UpdateWatcherSyncTime(id string, success bool, lastErr string) error {
	var query string
	if success {
		query = `
		UPDATE watchers
		SET last_success_sync_at = CURRENT_TIMESTAMP, status = 'HEALTHY', last_error_message = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ?;
		`
		_, err := s.db.Exec(query, id)
		return err
	}

	query = `
	UPDATE watchers
	SET last_failed_sync_at = CURRENT_TIMESTAMP, last_error_message = ?, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, lastErr, id)
	return err
}

// EnqueueJob insere atomicamente um novo Job com status PENDING na fila SQLite.
func (s *Store) EnqueueJob(job *Job) error {
	payloadJSON, err := json.Marshal(job.PayloadFiles)
	if err != nil {
		return fmt.Errorf("falha ao serializar payload de arquivos do job: %w", err)
	}

	if job.ScheduledFor.IsZero() {
		job.ScheduledFor = time.Now()
	}
	if job.MaxRetries <= 0 {
		job.MaxRetries = 5
	}
	job.Status = StatusPending

	query := `
	INSERT INTO jobs (id, watcher_id, pipeline_name, status, payload_files, retry_count, max_retries, scheduled_for, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);
	`
	_, err = s.db.Exec(query, job.ID, job.WatcherID, job.PipelineName, job.Status, string(payloadJSON), job.RetryCount, job.MaxRetries, job.ScheduledFor)
	if err != nil {
		return fmt.Errorf("falha ao enfileirar job '%s': %w", job.ID, err)
	}

	return nil
}

// DequeueNextPending aloca atomicamente o próximo job elegível marcando-o como RUNNING.
func (s *Store) DequeueNextPending(lockedBy string) (*Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("falha ao iniciar transação: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectQuery := `
	SELECT id, watcher_id, pipeline_name, status, payload_files, retry_count, max_retries, scheduled_for, created_at, updated_at
	FROM jobs
	WHERE status = 'PENDING' AND scheduled_for <= ?
	ORDER BY scheduled_for ASC, created_at ASC
	LIMIT 1;
	`

	var (
		j           Job
		payloadText string
	)

	err = tx.QueryRow(selectQuery, time.Now()).Scan(
		&j.ID,
		&j.WatcherID,
		&j.PipelineName,
		&j.Status,
		&payloadText,
		&j.RetryCount,
		&j.MaxRetries,
		&j.ScheduledFor,
		&j.CreatedAt,
		&j.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Nenhum job pendente
	}
	if err != nil {
		return nil, fmt.Errorf("falha ao consultar job pendente: %w", err)
	}

	if err := json.Unmarshal([]byte(payloadText), &j.PayloadFiles); err != nil {
		return nil, fmt.Errorf("falha ao deserializar arquivos do job: %w", err)
	}

	now := time.Now()
	updateQuery := `
	UPDATE jobs
	SET status = 'RUNNING', locked_by = ?, locked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err = tx.Exec(updateQuery, lockedBy, j.ID)
	if err != nil {
		return nil, fmt.Errorf("falha ao marcar job como RUNNING: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("falha ao efetivar dequeue do job: %w", err)
	}

	j.Status = StatusRunning
	j.LockedBy = lockedBy
	j.LockedAt = &now
	return &j, nil
}

// MarkJobCompleted finaliza o ciclo de vida do job com sucesso.
func (s *Store) MarkJobCompleted(jobID string) error {
	query := `
	UPDATE jobs
	SET status = 'COMPLETED', locked_by = NULL, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, jobID)
	if err != nil {
		return fmt.Errorf("falha ao marcar job '%s' como concluído: %w", jobID, err)
	}
	return nil
}

// MarkJobFailed atualiza o job para nova tentativa (PENDING com backoff) ou falha definitiva (FAILED).
func (s *Store) MarkJobFailed(jobID string, lastError string, canRetry bool, backoff time.Duration) error {
	var (
		currentRetry int
		maxRetries   int
	)

	err := s.db.QueryRow("SELECT retry_count, max_retries FROM jobs WHERE id = ?", jobID).Scan(&currentRetry, &maxRetries)
	if err != nil {
		return fmt.Errorf("falha ao consultar tentativas do job '%s': %w", jobID, err)
	}

	if canRetry && (currentRetry+1) < maxRetries {
		nextSchedule := time.Now().Add(backoff)
		query := `
		UPDATE jobs
		SET status = 'PENDING', retry_count = retry_count + 1, scheduled_for = ?, locked_by = NULL, locked_at = NULL, last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?;
		`
		_, err := s.db.Exec(query, nextSchedule, lastError, jobID)
		if err != nil {
			return fmt.Errorf("falha ao reagendar job '%s' para retry: %w", jobID, err)
		}
		return nil
	}

	// Tentativas esgotadas ou erro não recuperável
	query := `
	UPDATE jobs
	SET status = 'FAILED', last_error = ?, locked_by = NULL, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err = s.db.Exec(query, lastError, jobID)
	if err != nil {
		return fmt.Errorf("falha ao marcar job '%s' como falho definitivo: %w", jobID, err)
	}
	return nil
}

// MarkJobBlocked marca o job como bloqueado (ex.: por conflito insolúvel de merge).
func (s *Store) MarkJobBlocked(jobID string, reason string) error {
	query := `
	UPDATE jobs
	SET status = 'BLOCKED', last_error = ?, locked_by = NULL, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, reason, jobID)
	if err != nil {
		return fmt.Errorf("falha ao marcar job '%s' como bloqueado: %w", jobID, err)
	}
	return nil
}

// ListPendingJobs retorna a lista de jobs pendentes para um determinado watcher.
func (s *Store) ListPendingJobs(watcherID string) ([]*Job, error) {
	query := `
	SELECT id, watcher_id, pipeline_name, status, payload_files, retry_count, max_retries, scheduled_for, created_at, updated_at
	FROM jobs
	WHERE status = 'PENDING' AND (watcher_id = ? OR ? = '')
	ORDER BY scheduled_for ASC, created_at ASC;
	`
	rows, err := s.db.Query(query, watcherID, watcherID)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar jobs pendentes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ResetRunningJobs reverte jobs presos em RUNNING (ex.: após crash de energia) para PENDING.
func (s *Store) ResetRunningJobs() (int64, error) {
	query := `
	UPDATE jobs
	SET status = 'PENDING', locked_by = NULL, locked_at = NULL, updated_at = CURRENT_TIMESTAMP
	WHERE status = 'RUNNING';
	`
	res, err := s.db.Exec(query)
	if err != nil {
		return 0, fmt.Errorf("falha ao resetar jobs em RUNNING: %w", err)
	}
	return res.RowsAffected()
}

// RecordPipelineRun insere uma entrada de auditoria na tabela pipeline_runs.
func (s *Store) RecordPipelineRun(run *PipelineRun) error {
	query := `
	INSERT INTO pipeline_runs (id, job_id, watcher_id, pipeline_name, status, duration_ms, error_step, error_details, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP);
	`
	_, err := s.db.Exec(query, run.ID, run.JobID, run.WatcherID, run.PipelineName, run.Status, run.DurationMs, run.ErrorStep, run.ErrorDetails)
	if err != nil {
		return fmt.Errorf("falha ao registrar execução de pipeline: %w", err)
	}
	return nil
}

func scanWatcher(row *sql.Row) (*WatcherRecord, error) {
	var w WatcherRecord
	err := row.Scan(
		&w.ID,
		&w.Name,
		&w.Path,
		&w.Status,
		&w.LastEventAt,
		&w.LastSuccessSyncAt,
		&w.LastFailedSyncAt,
		&w.LastErrorMessage,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func scanWatcherRow(rows *sql.Rows) (*WatcherRecord, error) {
	var w WatcherRecord
	err := rows.Scan(
		&w.ID,
		&w.Name,
		&w.Path,
		&w.Status,
		&w.LastEventAt,
		&w.LastSuccessSyncAt,
		&w.LastFailedSyncAt,
		&w.LastErrorMessage,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func scanJobRow(rows *sql.Rows) (*Job, error) {
	var (
		j           Job
		payloadText string
	)
	err := rows.Scan(
		&j.ID,
		&j.WatcherID,
		&j.PipelineName,
		&j.Status,
		&payloadText,
		&j.RetryCount,
		&j.MaxRetries,
		&j.ScheduledFor,
		&j.CreatedAt,
		&j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(payloadText), &j.PayloadFiles); err != nil {
		return nil, fmt.Errorf("falha ao deserializar arquivos: %w", err)
	}
	return &j, nil
}
