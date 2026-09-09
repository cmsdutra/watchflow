package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/watchflow/watchflow/internal/logger"
)

// maxSocketPathLen é o tamanho do campo sun_path de sockaddr_un, incluindo o
// terminador nulo — 108 bytes no Linux e também na implementação AF_UNIX do
// Windows 10 1803+. Caminhos com 108 bytes ou mais são rejeitados no bind.
const maxSocketPathLen = 108

// Handler define as operações que o daemon deve prover para responder às requisições IPC.
type Handler interface {
	Status(ctx context.Context) (*StatusResponse, error)
	Jobs(ctx context.Context, watcherName string, limit int) (*JobsResponse, error)
	Runs(ctx context.Context, watcherName string, limit int) (*RunsResponse, error)
	Reload(ctx context.Context) (*ReloadResponse, error)
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
	log        *slog.Logger
	ready      chan struct{}
	readyOnce  sync.Once
	mu         sync.Mutex
	closed     bool
	wg         sync.WaitGroup
}

// NewServer inicializa uma nova instância do servidor IPC.
func NewServer(socketPath string, handler Handler) *Server {
	return &Server{
		socketPath: socketPath,
		handler:    handler,
		log:        logger.For("ipc"),
		ready:      make(chan struct{}),
	}
}

// Start inicia a escuta no Unix Domain Socket e atende conexões até o cancelamento do contexto.
func (s *Server) Start(ctx context.Context) error {
	if s.socketPath == "" {
		return fmt.Errorf("caminho do socket Unix não pode ser vazio")
	}

	cleanPath := filepath.Clean(s.socketPath)

	// O endereço de um socket Unix vive no campo sun_path de sockaddr_un, de
	// tamanho fixo — e o Windows usa a mesma estrutura de 108 bytes. Estourar o
	// limite falha no bind com 'invalid argument', que não diz nada sobre o
	// tamanho do caminho. Checar antes troca esse erro opaco por um acionável.
	if len(cleanPath) >= maxSocketPathLen {
		return fmt.Errorf(
			"caminho do socket Unix tem %d bytes e excede o limite de %d imposto pelo sistema operacional: '%s'; "+
				"configure 'daemon.socket_path' para um diretório mais curto",
			len(cleanPath), maxSocketPathLen-1, cleanPath)
	}

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
		s.log.Warn("socket órfão de execução anterior removido", slog.String("socket", cleanPath))
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

	s.log.Info("servidor IPC escutando", slog.String("socket", cleanPath))
	s.readyOnce.Do(func() { close(s.ready) })

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
			s.log.Error("falha ao aceitar conexão no socket", slog.Any("error", acceptErr))
			return acceptErr
		}

		// O registro no WaitGroup precisa ser serializado com Close(): um Add
		// concorrente com o Wait() de Close é uso indevido de sync.WaitGroup.
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return nil
		}
		s.wg.Add(1)
		s.mu.Unlock()

		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			s.handleConnection(ctx, c)
		}(conn)
	}
}

// Ready fecha assim que o socket está vinculado e aceitando conexões. Permite ao
// chamador confirmar a trava de processo único de forma determinística, em vez
// de presumir sucesso após uma espera arbitrária.
func (s *Server) Ready() <-chan struct{} {
	return s.ready
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
			s.log.Warn("requisição JSON-RPC malformada", slog.Any("error", err))
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
	s.log.Debug("requisição IPC recebida", slog.String("metodo", req.Method))

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

	case "jobs":
		var p ListRequest
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		res, err := s.handler.Jobs(ctx, p.WatcherName, p.Limit)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "runs":
		var p ListRequest
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &p)
		}
		res, err := s.handler.Runs(ctx, p.WatcherName, p.Limit)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			return resp
		}
		data, _ := json.Marshal(res)
		resp.Result = data

	case "reload":
		res, err := s.handler.Reload(ctx)
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
