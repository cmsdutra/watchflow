package normalizer

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxHashableSize limita o custo de hashear arquivos grandes; acima disso a
// entrada recai para supressão por caminho.
const maxHashableSize = 8 << 20 // 8 MiB

type suppressedItem struct {
	expiresAt time.Time

	// hash é o conteúdo que o WatchFlow gravou. Quando presente, o evento só é
	// suprimido se o arquivo AINDA tiver esse conteúdo: se o usuário editou o
	// mesmo arquivo dentro da janela, o evento é legítimo e deve passar.
	hash string
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

// SuppressContent registra o caminho junto com o hash do seu conteúdo atual.
// Diferente de Suppress, que cega o filtro para o caminho durante toda a janela,
// esta variante só descarta eventos enquanto o arquivo permanecer exatamente
// como o daemon o deixou.
func (s *EchoSuppressor) SuppressContent(path string, window time.Duration) {
	if path == "" {
		return
	}
	if window <= 0 {
		window = s.defaultWindow
	}

	clean := canonicalPath(path)
	hash, err := hashFile(clean)
	if err != nil {
		// Não foi possível ler (arquivo removido pela operação): recai para
		// supressão por caminho, que é o comportamento correto para remoções.
		s.Suppress(path, window)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[clean] = suppressedItem{expiresAt: time.Now().Add(window), hash: hash}
}

// SuppressContentMultiple aplica SuppressContent a um conjunto de caminhos.
func (s *EchoSuppressor) SuppressContentMultiple(paths []string, window time.Duration) {
	for _, p := range paths {
		s.SuppressContent(p, window)
	}
}

// IsSuppressed verifica se o caminho informado está atualmente sob a janela de supressão de eco.
func (s *EchoSuppressor) IsSuppressed(path string) bool {
	if path == "" {
		return false
	}

	now := time.Now()

	// Caminho rápido: fora de uma sincronização o catálogo está vazio, e este é
	// o caso da esmagadora maioria dos eventos. Evita resolver symlinks (um
	// lstat por componente do caminho) a cada evento do kernel.
	s.mu.RLock()
	empty := len(s.items) == 0
	item, exists := s.items[filepath.Clean(path)]
	s.mu.RUnlock()

	if empty {
		return false
	}

	clean := filepath.Clean(path)
	if !exists {
		// Só resolve o caminho canônico se a busca direta falhou
		clean = canonicalPath(path)
		s.mu.RLock()
		item, exists = s.items[clean]
		s.mu.RUnlock()
	}

	if !exists {
		return false
	}

	if now.Before(item.expiresAt) {
		if item.hash == "" {
			return true
		}

		// Conteúdo idêntico ao que o daemon gravou: é eco.
		// Conteúdo diferente: o usuário alterou o arquivo e o evento é real.
		current, err := hashFile(clean)
		if err != nil {
			return true // arquivo sumiu: eco de remoção feita pelo daemon
		}
		return current == item.hash
	}

	// Limpeza sob demanda do item expirado
	s.mu.Lock()
	if current, ok := s.items[clean]; ok && now.After(current.expiresAt) {
		delete(s.items, clean)
	}
	s.mu.Unlock()

	return false
}

// hashFile calcula o SHA-256 do conteúdo do arquivo.
func hashFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Size() > maxHashableSize {
		return "", os.ErrInvalid
	}

	f, err := os.Open(path) // #nosec G304 -- caminho vem da árvore vigiada
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// PurgeExpired remove as entradas vencidas. Sem uma varredura periódica, um
// caminho suprimido e nunca mais consultado permanecia no mapa indefinidamente,
// já que a limpeza só acontecia ao consultar aquele caminho específico.
// Retorna quantas entradas foram descartadas.
func (s *EchoSuppressor) PurgeExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	removed := 0
	for p, item := range s.items {
		if now.After(item.expiresAt) {
			delete(s.items, p)
			removed++
		}
	}
	return removed
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
