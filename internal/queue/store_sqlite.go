package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Todas as configurações vão no DSN, e não via db.Exec("PRAGMA ..."):
	// database/sql mantém um POOL de conexões, e um PRAGMA executado por Exec
	// atinge apenas a conexão que atendeu aquela chamada. As demais nasciam sem
	// busy_timeout, então a contenção entre workers virava "database is locked"
	// imediato em vez de aguardar a trava.
	//
	// _txlock=immediate faz BEGIN IMMEDIATE: a transação já nasce com a trava de
	// escrita. Com o padrão (deferred), dois workers selecionavam a MESMA linha
	// e o segundo levava SQLITE_BUSY_SNAPSHOT ao promover para escrita.
	params := strings.Join([]string{
		"_txlock=immediate",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(ON)",
	}, "&")

	dsn := "file:" + dbPath + "?" + params
	if dbPath == ":memory:" {
		dsn = "file::memory:?cache=shared&" + params
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("falha ao abrir banco de dados SQLite '%s': %w", dbPath, err)
	}

	// O SQLite admite um único escritor por vez; um pool grande apenas multiplica
	// a contenção sem ganho de vazão.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("falha ao conectar ao banco '%s': %w", dbPath, err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("falha ao inicializar schema do banco: %w", err)
	}

	return &Store{db: db}, nil
}

// Close encerra a conexão com o banco SQLite.
//
// Antes de fechar, força um checkpoint TRUNCATE. O autocheckpoint padrão (1000
// páginas) recicla o WAL no lugar, sem nunca reduzir o arquivo: depois de
// algumas horas de operação ele estaciona na marca d'água de ~4 MB mesmo com o
// banco em algumas centenas de KB. TRUNCATE zera o arquivo, o que mantém o
// state_dir enxuto e encurta a recuperação no próximo start.
//
// É best-effort: o checkpoint falha se ainda houver leitor ativo, e nesse caso
// perder a truncagem é preferível a reportar um erro de encerramento que não
// afeta a durabilidade — o WAL continua válido e é reaplicado no próximo start.
func (s *Store) Close() error {
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
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

// RemoveWatcher exclui definitivamente o registro de um watcher, seus jobs
// enfileirados (por cascata via ON DELETE CASCADE em jobs.watcher_id) e seu
// histórico de execuções. pipeline_runs precisa ser limpo explicitamente antes:
// job_id ali referencia jobs(id) sem cascade, então excluir os jobs primeiro
// deixaria referências soltas e o FOREIGN KEY constraint da própria tabela
// watchers bloquearia a operação inteira.
//
// Usado quando um watcher sai da configuração (reload a quente ou reinício com
// config editada de fora), para que 'status' e 'jobs' parem de reportar um
// repositório que não é mais vigiado.
func (s *Store) RemoveWatcher(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("falha ao iniciar transação para remover watcher '%s': %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM pipeline_runs WHERE watcher_id = ?;`, id); err != nil {
		return fmt.Errorf("falha ao remover histórico de execuções do watcher '%s': %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM watchers WHERE id = ?;`, id); err != nil {
		return fmt.Errorf("falha ao remover watcher '%s': %w", id, err)
	}
	return tx.Commit()
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

// UpdateWatcherEventTime atualiza o timestamp do último evento detectado no watcher.
func (s *Store) UpdateWatcherEventTime(id string) error {
	query := `
	UPDATE watchers
	SET last_event_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, id)
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

// RequeueJob devolve um job para PENDING sem consumir uma tentativa de retry.
// Usado quando a execução foi adiada por uma condição externa ao job em si
// (ex.: watcher pausado pelo usuário), que não representa uma falha real.
func (s *Store) RequeueJob(jobID string, delay time.Duration, reason string) error {
	query := `
	UPDATE jobs
	SET status = 'PENDING', scheduled_for = ?, locked_by = NULL, locked_at = NULL, last_error = ?, updated_at = CURRENT_TIMESTAMP
	WHERE id = ?;
	`
	_, err := s.db.Exec(query, time.Now().Add(delay), reason, jobID)
	if err != nil {
		return fmt.Errorf("falha ao reenfileirar job '%s': %w", jobID, err)
	}
	return nil
}

// RequeueRunningJobs devolve para PENDING todos os jobs ainda em RUNNING sem
// consumir tentativas de retry. Deve ser chamado no encerramento gracioso: como
// a parada foi ordenada e não uma falha, o job não pode ser penalizado.
// Difere de ResetRunningJobs, que trata queda abrupta e incrementa retry_count
// para preservar a proteção contra jobs venenosos.
func (s *Store) RequeueRunningJobs(reason string) (int64, error) {
	query := `
	UPDATE jobs
	SET status = 'PENDING', locked_by = NULL, locked_at = NULL, last_error = ?, updated_at = CURRENT_TIMESTAMP
	WHERE status = 'RUNNING';
	`
	res, err := s.db.Exec(query, reason)
	if err != nil {
		return 0, fmt.Errorf("falha ao reenfileirar jobs em RUNNING: %w", err)
	}
	return res.RowsAffected()
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

// DefaultRetention é a janela padrão de histórico preservado.
const DefaultRetention = 7 * 24 * time.Hour

// PurgeResult resume o que foi removido em uma passagem de retenção.
type PurgeResult struct {
	Jobs int64
	Runs int64
}

// PurgeOldRecords remove jobs terminais e execuções de pipeline mais antigos que
// a janela informada. Sem isso o state.db cresce indefinidamente: com debounce
// de 15s, um único watcher pode gerar milhares de linhas por dia, degradando
// progressivamente o dequeue e ocupando disco sem limite.
//
// Jobs em PENDING, RUNNING ou BLOCKED nunca são removidos: representam trabalho
// não concluído ou conflitos aguardando o usuário.
func (s *Store) PurgeOldRecords(retention time.Duration) (*PurgeResult, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}

	// created_at/updated_at são gravados pelo SQLite via CURRENT_TIMESTAMP, que
	// é UTC. Comparar com um time.Time do Go (local) desloca o corte pelo offset
	// do fuso: em UTC-3 nada seria purgado, e em UTC+9 histórico recente seria
	// apagado cedo demais. Calcular o corte dentro do próprio SQLite mantém os
	// dois lados no mesmo relógio e no mesmo formato.
	cutoff := fmt.Sprintf("-%d seconds", int64(retention.Seconds()))

	res := &PurgeResult{}

	// pipeline_runs.job_id referencia jobs(id) sem ON DELETE, então a remoção de
	// um job que ainda tenha execuções apontando para ele violaria a foreign key
	// e abortaria a manutenção inteira. As execuções saem primeiro: por idade e,
	// em seguida, as que pertencem aos jobs prestes a serem removidos.
	runRes, err := s.db.Exec(
		`DELETE FROM pipeline_runs WHERE created_at < datetime('now', ?);`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("falha ao purgar histórico de execuções: %w", err)
	}
	res.Runs, _ = runRes.RowsAffected()

	orphanRes, err := s.db.Exec(`
		DELETE FROM pipeline_runs
		WHERE job_id IN (
			SELECT id FROM jobs
			WHERE status IN ('COMPLETED', 'FAILED') AND updated_at < datetime('now', ?)
		);`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("falha ao purgar execuções de jobs expirados: %w", err)
	}
	if n, _ := orphanRes.RowsAffected(); n > 0 {
		res.Runs += n
	}

	jobRes, err := s.db.Exec(
		`DELETE FROM jobs WHERE status IN ('COMPLETED', 'FAILED') AND updated_at < datetime('now', ?);`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("falha ao purgar jobs concluídos: %w", err)
	}
	res.Jobs, _ = jobRes.RowsAffected()

	return res, nil
}

// CountJobsByStatus retorna a quantidade de jobs agrupada por status.
func (s *Store) CountJobsByStatus() (map[JobStatus]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status;`)
	if err != nil {
		return nil, fmt.Errorf("falha ao contar jobs por status: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[JobStatus]int)
	for rows.Next() {
		var (
			status JobStatus
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

// DefaultListLimit limita consultas de listagem quando nenhum limite é informado.
const DefaultListLimit = 50

// ListRecentJobs retorna os jobs mais recentemente atualizados, de qualquer
// status. Diferente de ListPendingJobs, serve à inspeção do estado da fila
// (CLI e TUI), por isso inclui last_error e o momento da última atualização.
func (s *Store) ListRecentJobs(watcherID string, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}

	query := `
	SELECT id, watcher_id, pipeline_name, status, payload_files, retry_count, max_retries,
	       scheduled_for, COALESCE(last_error, ''), created_at, updated_at
	FROM jobs
	WHERE (watcher_id = ? OR ? = '')
	ORDER BY updated_at DESC, created_at DESC
	LIMIT ?;
	`
	rows, err := s.db.Query(query, watcherID, watcherID, limit)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar jobs recentes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var jobs []*Job
	for rows.Next() {
		var (
			j           Job
			payloadText string
		)
		if err := rows.Scan(
			&j.ID, &j.WatcherID, &j.PipelineName, &j.Status, &payloadText,
			&j.RetryCount, &j.MaxRetries, &j.ScheduledFor, &j.LastError,
			&j.CreatedAt, &j.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payloadText), &j.PayloadFiles); err != nil {
			return nil, fmt.Errorf("falha ao deserializar arquivos do job '%s': %w", j.ID, err)
		}
		jobs = append(jobs, &j)
	}
	return jobs, rows.Err()
}

// ListRecentRuns retorna o histórico de auditoria mais recente.
func (s *Store) ListRecentRuns(watcherID string, limit int) ([]*PipelineRun, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}

	query := `
	SELECT id, COALESCE(job_id, ''), watcher_id, pipeline_name, status,
	       COALESCE(duration_ms, 0), COALESCE(error_step, ''), COALESCE(error_details, ''), created_at
	FROM pipeline_runs
	WHERE (watcher_id = ? OR ? = '')
	ORDER BY created_at DESC, id DESC
	LIMIT ?;
	`
	rows, err := s.db.Query(query, watcherID, watcherID, limit)
	if err != nil {
		return nil, fmt.Errorf("falha ao listar execuções recentes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runs []*PipelineRun
	for rows.Next() {
		var r PipelineRun
		if err := rows.Scan(
			&r.ID, &r.JobID, &r.WatcherID, &r.PipelineName, &r.Status,
			&r.DurationMs, &r.ErrorStep, &r.ErrorDetails, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		runs = append(runs, &r)
	}
	return runs, rows.Err()
}

// ResetRunningJobs reverte jobs presos em RUNNING (ex.: após crash de energia) para PENDING ou FAILED se esgotar tentativas.
func (s *Store) ResetRunningJobs() (int64, error) {
	query := `
	UPDATE jobs
	SET status = CASE
			WHEN retry_count + 1 >= max_retries THEN 'FAILED'
			ELSE 'PENDING'
		END,
		retry_count = retry_count + 1,
		locked_by = NULL,
		locked_at = NULL,
		last_error = 'daemon reiniciado durante execução anterior (recuperado pós-crash)',
		updated_at = CURRENT_TIMESTAMP
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
	// job_id é anulável no schema e tem foreign key para jobs(id): gravar string
	// vazia violaria a constraint em vez de representar "sem job associado".
	var jobID any
	if run.JobID != "" {
		jobID = run.JobID
	}

	_, err := s.db.Exec(query, run.ID, jobID, run.WatcherID, run.PipelineName, run.Status, run.DurationMs, run.ErrorStep, run.ErrorDetails)
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
