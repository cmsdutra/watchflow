package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Handler define as operações que o daemon deve prover para responder às requisições IPC.
type Handler interface {
	Status(ctx context.Context) (*StatusResponse, error)
	Sync(ctx context.Context, watcherName string) (*SyncResponse, error)
	Pause(ctx context.Context, watcherName string) (*ActionResponse, error)
	Resume(ctx context.Context, watcherName string) (*ActionResponse, error)
	Stop(ctx context.Context) (*ActionResponse, error)
}

// Server gerencia o Unix Domain Socket e o despacho de requisições JSON-RPC do daemon.
type Server struct {
	socketPath string
	handler    Handler
	listener   net.Listener
	mu         sync.Mutex
	closed     bool
	wg         sync.WaitGroup
}

// NewServer inicializa uma nova instância do servidor IPC.
func NewServer(socketPath string, handler Handler) *Server {
	return &Server{
		socketPath: socketPath,
		handler:    handler,
	}
}

// Start inicia a escuta no Unix Domain Socket e atende conexões até o cancelamento do contexto.
func (s *Server) Start(ctx context.Context) error {
	if s.socketPath == "" {
		return fmt.Errorf("caminho do socket Unix não pode ser vazio")
	}

	cleanPath := filepath.Clean(s.socketPath)
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("falha ao criar diretório para o socket Unix '%s': %w", dir, err)
	}

	// 1. Process Lock: Verifica se outra instância viva já está ouvindo no socket
	if _, err := os.Stat(cleanPath); err == nil {
		conn, dialErr := net.DialTimeout("unix", cleanPath, 500*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return fmt.Errorf("outra instância do daemon já está em execução no socket '%s'", cleanPath)
		}
		// Socket órfão de crash anterior: remoção limpa
		_ = os.Remove(cleanPath)
	}

	l, err := net.Listen("unix", cleanPath)
	if err != nil {
		return fmt.Errorf("falha ao abrir socket Unix '%s': %w", cleanPath, err)
	}

	// Permissões restritas ao usuário corrente
	_ = os.Chmod(cleanPath, 0600)

	s.mu.Lock()
	s.listener = l
	s.closed = false
	s.mu.Unlock()

	// Encerramento limpo quando o contexto for cancelado
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	for {
		conn, acceptErr := l.Accept()
		if acceptErr != nil {
			s.mu.Lock()
			isClosed := s.closed
			s.mu.Unlock()
			if isClosed {
				return nil
			}
			return acceptErr
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			s.handleConnection(ctx, c)
		}(conn)
	}
}

// Close encerra a escuta do servidor e remove o arquivo de socket do filesystem.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()
	_ = os.Remove(s.socketPath)
	return err
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			_ = enc.Encode(Response{
				JSONRPC: "2.0",
				Error: &RPCError{
					Code:    -32700,
					Message: fmt.Sprintf("falha ao decodificar JSON: %v", err),
				},
			})
			return
		}

		resp := s.dispatch(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	resp := Response{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	if s.handler == nil {
		resp.Error = &RPCError{Code: -32603, Message: "nenhum handler configurado no servidor"}
		return resp
	}

	switch req.Method {
	case "status":
		res, err := s.handler.Status(ctx)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "sync":
		var p SyncRequest
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		res, err := s.handler.Sync(ctx, p.WatcherName)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "pause":
		var p ActionRequest
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		res, err := s.handler.Pause(ctx, p.WatcherName)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "resume":
		var p ActionRequest
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		res, err := s.handler.Resume(ctx, p.WatcherName)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "stop":
		res, err := s.handler.Stop(ctx)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	default:
		resp.Error = &RPCError{
			Code:    -32601,
			Message: fmt.Sprintf("método '%s' não encontrado", req.Method),
		}
	}

	return resp
}
