package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// Client provê acesso ergonômico aos serviços IPC do daemon do WatchFlow.
type Client struct {
	socketPath string
	timeout    time.Duration
	reqID      uint64
}

// NewClient inicializa um cliente configurado para o caminho do socket Unix.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    5 * time.Second,
	}
}

// SetTimeout ajusta o tempo limite padrão para chamadas RPC.
func (c *Client) SetTimeout(t time.Duration) {
	if t > 0 {
		c.timeout = t
	}
}

// IsDaemonRunning verifica de forma não bloqueante se o daemon está ativo e respondendo.
func (c *Client) IsDaemonRunning() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Call envia uma requisição JSON-RPC 2.0 e decodifica o resultado.
func (c *Client) Call(ctx context.Context, method string, params interface{}, result interface{}) error {
	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("não foi possível conectar ao daemon do WatchFlow em '%s': %w (o daemon está em execução?)", c.socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("falha ao serializar parâmetros: %w", err)
		}
		paramsRaw = b
	}

	reqID := atomic.AddUint64(&c.reqID, 1)
	req := Request{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  method,
		Params:  paramsRaw,
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("falha ao enviar requisição IPC: %w", err)
	}

	dec := json.NewDecoder(conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("falha ao ler resposta do daemon: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("erro retornado pelo daemon (código %d): %s", resp.Error.Code, resp.Error.Message)
	}

	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("falha ao desserializar resultado: %w", err)
		}
	}

	return nil
}

// Status consulta o estado detalhado do daemon e métricas de todos os watchers.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.Call(ctx, "status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Jobs consulta o estado da fila persistente do daemon.
func (c *Client) Jobs(ctx context.Context, watcherName string, limit int) (*JobsResponse, error) {
	var res JobsResponse
	if err := c.Call(ctx, "jobs", ListRequest{WatcherName: watcherName, Limit: limit}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Runs consulta o histórico de auditoria de execuções de pipeline.
func (c *Client) Runs(ctx context.Context, watcherName string, limit int) (*RunsResponse, error) {
	var res RunsResponse
	if err := c.Call(ctx, "runs", ListRequest{WatcherName: watcherName, Limit: limit}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Sync solicita a sincronização imediata de um watcher ou de todos se watcherName for vazio.
func (c *Client) Sync(ctx context.Context, watcherName string) (*SyncResponse, error) {
	var resp SyncResponse
	req := SyncRequest{WatcherName: watcherName}
	if err := c.Call(ctx, "sync", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Pause pausa o monitoramento e execução de pipelines de um watcher.
func (c *Client) Pause(ctx context.Context, watcherName string) (*ActionResponse, error) {
	var resp ActionResponse
	req := ActionRequest{WatcherName: watcherName}
	if err := c.Call(ctx, "pause", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Resume retoma a operação regular de um watcher pausado ou halted.
func (c *Client) Resume(ctx context.Context, watcherName string) (*ActionResponse, error) {
	var resp ActionResponse
	req := ActionRequest{WatcherName: watcherName}
	if err := c.Call(ctx, "resume", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Stop solicita o desligamento ordenado (graceful shutdown) do daemon.
func (c *Client) Stop(ctx context.Context) (*ActionResponse, error) {
	var resp ActionResponse
	if err := c.Call(ctx, "stop", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
