package locking

import (
	"context"
	"path/filepath"
	"sync"
	"time"
)

// RepoLocker gerencia travas mútuas em memória por caminho canônico de repositório,
// garantindo que múltiplos jobs concorrentes não operem na mesma árvore Git.
type RepoLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewRepoLocker cria uma nova instância do gerenciador de locks por repositório.
func NewRepoLocker() *RepoLocker {
	return &RepoLocker{
		locks: make(map[string]*sync.Mutex),
	}
}

// DefaultRepoLocker é a instância global de travas de repositório.
var DefaultRepoLocker = NewRepoLocker()

// Lock adquire a trava exclusiva para o repositório indicado pelo caminho canônico.
// Retorna uma função de unlock para ser executada via defer.
func (rl *RepoLocker) Lock(path string) func() {
	cleanPath := canonicalPath(path)

	rl.mu.Lock()
	m, exists := rl.locks[cleanPath]
	if !exists {
		m = &sync.Mutex{}
		rl.locks[cleanPath] = m
	}
	rl.mu.Unlock()

	m.Lock()
	return func() {
		m.Unlock()
	}
}

// LockContext adquire a trava exclusiva do repositório respeitando o cancelamento
// do contexto, evitando que um worker fique preso indefinidamente aguardando um
// pipeline concorrente durante o encerramento do daemon.
func (rl *RepoLocker) LockContext(ctx context.Context, path string) (func(), error) {
	if unlock, ok := rl.TryLock(path); ok {
		return unlock, nil
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if unlock, ok := rl.TryLock(path); ok {
				return unlock, nil
			}
		}
	}
}

// TryLock tenta adquirir a trava do repositório sem bloquear.
// Retorna a função de unlock e true se adquirida, ou nil e false se já estiver ocupada.
func (rl *RepoLocker) TryLock(path string) (func(), bool) {
	cleanPath := canonicalPath(path)

	rl.mu.Lock()
	m, exists := rl.locks[cleanPath]
	if !exists {
		m = &sync.Mutex{}
		rl.locks[cleanPath] = m
	}
	rl.mu.Unlock()

	if m.TryLock() {
		return func() {
			m.Unlock()
		}, true
	}

	return nil, false
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	realPath, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return realPath
	}
	return clean
}
