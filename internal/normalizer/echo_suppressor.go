package normalizer

import (
	"path/filepath"
	"sync"
	"time"
)

type suppressedItem struct {
	expiresAt time.Time
}

// EchoSuppressor mantém um catálogo temporário em memória de arquivos manipulados internamente
// pelo WatchFlow (ex.: durante git merge ou pull) para evitar ciclos recursivos de commits.
type EchoSuppressor struct {
	mu            sync.RWMutex
	items         map[string]suppressedItem
	defaultWindow time.Duration
}

// NewEchoSuppressor inicializa um supressor de eco com janela temporal configurada (padrão: 2s).
func NewEchoSuppressor(defaultWindow time.Duration) *EchoSuppressor {
	if defaultWindow <= 0 {
		defaultWindow = 2 * time.Second
	}
	return &EchoSuppressor{
		items:         make(map[string]suppressedItem),
		defaultWindow: defaultWindow,
	}
}

// DefaultEchoSuppressor é a instância global de supressão de eco do WatchFlow.
var DefaultEchoSuppressor = NewEchoSuppressor(2 * time.Second)

// Suppress registra um caminho absoluto para ser ignorado temporariamente pelo filtro de eventos.
func (s *EchoSuppressor) Suppress(path string, window time.Duration) {
	if path == "" {
		return
	}
	if window <= 0 {
		window = s.defaultWindow
	}

	clean := canonicalPath(path)
	expiresAt := time.Now().Add(window)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[clean] = suppressedItem{expiresAt: expiresAt}
}

// SuppressMultiple registra múltiplos caminhos em lote para supressão temporária.
func (s *EchoSuppressor) SuppressMultiple(paths []string, window time.Duration) {
	if len(paths) == 0 {
		return
	}
	if window <= 0 {
		window = s.defaultWindow
	}

	expiresAt := time.Now().Add(window)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range paths {
		if p == "" {
			continue
		}
		clean := canonicalPath(p)
		s.items[clean] = suppressedItem{expiresAt: expiresAt}
	}
}

// IsSuppressed verifica se o caminho informado está atualmente sob a janela de supressão de eco.
func (s *EchoSuppressor) IsSuppressed(path string) bool {
	if path == "" {
		return false
	}

	clean := canonicalPath(path)
	now := time.Now()

	s.mu.RLock()
	item, exists := s.items[clean]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	if now.Before(item.expiresAt) {
		return true
	}

	// Limpeza sob demanda do item expirado
	s.mu.Lock()
	if current, ok := s.items[clean]; ok && now.After(current.expiresAt) {
		delete(s.items, clean)
	}
	s.mu.Unlock()

	return false
}

// Count retorna a quantidade de itens ativos (não expirados) no catálogo.
func (s *EchoSuppressor) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for p, item := range s.items {
		if now.After(item.expiresAt) {
			delete(s.items, p)
		}
	}
	return len(s.items)
}

// Clear limpa todos os registros de supressão.
func (s *EchoSuppressor) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make(map[string]suppressedItem)
}

func canonicalPath(p string) string {
	clean := filepath.Clean(p)
	realPath, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return realPath
	}
	return clean
}
