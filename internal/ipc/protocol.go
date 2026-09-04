package ipc

import (
	"encoding/json"
)

// Request representa uma mensagem JSON-RPC 2.0 enviada pelo cliente.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response representa a resposta JSON-RPC 2.0 enviada pelo servidor.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError detalha erros ocorridos durante o processamento da chamada RPC.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// StatusResponse consolida as métricas e estado do daemon para 'watchflow status'.
type StatusResponse struct {
	DaemonPID   int                `json:"daemon_pid"`
	Uptime      string             `json:"uptime"`
	Version     string             `json:"version"`
	Watchers    []WatcherStatusDTO `json:"watchers"`
	PendingJobs int                `json:"pending_jobs"`
	RunningJobs int                `json:"running_jobs"`
	BlockedJobs int                `json:"blocked_jobs"`
}

// WatcherStatusDTO reflete o estado operacional de um watcher para exibição na CLI.
type WatcherStatusDTO struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path"`
	Status            string `json:"status"`
	LastEventAt       string `json:"last_event_at,omitempty"`
	LastSuccessSyncAt string `json:"last_success_sync_at,omitempty"`
	LastFailedSyncAt  string `json:"last_failed_sync_at,omitempty"`
	LastError         string `json:"last_error,omitempty"`
}

// SyncRequest parâmetros para o comando sync.
type SyncRequest struct {
	WatcherName string `json:"watcher_name,omitempty"`
}

// SyncResponse resultado da solicitação de sincronização imediata.
type SyncResponse struct {
	EnqueuedJobs []string `json:"enqueued_jobs"`
	Message      string   `json:"message"`
}

// ActionRequest parâmetros para comandos como pause e resume.
type ActionRequest struct {
	WatcherName string `json:"watcher_name,omitempty"`
}

// ActionResponse resultado de comandos operacionais (pause, resume, stop).
type ActionResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}
