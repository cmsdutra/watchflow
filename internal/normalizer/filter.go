package normalizer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"github.com/watchflow/watchflow/internal/watcher"
)

// DefaultIgnorePatterns lista os padrões de ruído universais ignorados por padrão.
var DefaultIgnorePatterns = []string{
	".git",
	".git/**",
	"**/.git",
	"**/.git/**",
	"*.tmp",
	"**/*.tmp",
	".tmp/**",
	"**/.tmp/**",
	"*~",
	"**/*~",
	".DS_Store",
	"**/.DS_Store",
	"*.swp",
	"**/*.swp",
	".obsidian/cache/**",
	"**/.obsidian/cache/**",
}

// NormalizedEvent representa um evento de sistema de arquivos padronizado e validado.
type NormalizedEvent struct {
	WatcherName string          `json:"watcher_name"`
	AbsPath     string          `json:"abs_path"`
	RelPath     string          `json:"rel_path"`
	Op          watcher.EventOp `json:"op"`
	Timestamp   time.Time       `json:"timestamp"`
	IsDir       bool            `json:"is_dir"`
}

// Filter gerencia o casamento de padrões e normalização de eventos para um diretório raiz.
type Filter struct {
	baseDir        string
	compiledGlobs  []*glob.Pattern
	patterns       []string
	echoSuppressor *EchoSuppressor
}

// NewFilter inicializa o motor de filtragem compilando as regras glob fornecidas.
func NewFilter(baseDir string, userPatterns []string) (*Filter, error) {
	cleanBase := filepath.Clean(baseDir)
	canonicalBase, err := filepath.EvalSymlinks(cleanBase)
	if err == nil {
		cleanBase = canonicalBase
	}

	rawPatterns := make([]string, 0, len(DefaultIgnorePatterns)+len(userPatterns))
	rawPatterns = append(rawPatterns, DefaultIgnorePatterns...)
	rawPatterns = append(rawPatterns, userPatterns...)

	seen := make(map[string]bool)
	uniquePatterns := make([]string, 0, len(rawPatterns))
	compiled := make([]*glob.Pattern, 0, len(rawPatterns))

	for _, p := range rawPatterns {
		norm := strings.TrimSpace(filepath.ToSlash(p))
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true

		g, err := glob.Compile(norm, '/')
		if err != nil {
			return nil, fmt.Errorf("padrão glob inválido '%s': %w", norm, err)
		}

		uniquePatterns = append(uniquePatterns, norm)
		compiled = append(compiled, g)
	}

	return &Filter{
		baseDir:        cleanBase,
		compiledGlobs:  compiled,
		patterns:       uniquePatterns,
		echoSuppressor: DefaultEchoSuppressor,
	}, nil
}

// SetEchoSuppressor define uma instância personalizada de supressor de eco para o filtro.
func (f *Filter) SetEchoSuppressor(es *EchoSuppressor) {
	f.echoSuppressor = es
}

// ShouldIgnore avalia se o caminho especificado deve ser descartado por casamento com regras de ignore ou supressão de eco.
func (f *Filter) ShouldIgnore(absPath string) bool {
	cleanPath := filepath.Clean(absPath)
	slashPath := filepath.ToSlash(cleanPath)

	// Proteção de segurança: eventos fora da raiz monitorada são rejeitados
	rel, err := filepath.Rel(f.baseDir, cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return true
	}

	relSlash := filepath.ToSlash(rel)
	baseName := filepath.Base(cleanPath)

	// Os globs são puramente aritméticos sobre strings e eliminam a esmagadora
	// maioria dos eventos (.git/**, *.tmp). São avaliados PRIMEIRO para que o
	// ruído nunca chegue à supressão de eco, que faz stat/leitura de disco.
	for _, g := range f.compiledGlobs {
		// Casamento relativo (ex: .git/index ou sub/arquivo.tmp)
		if g.Match(relSlash) {
			return true
		}
		// Casamento pelo nome base (ex: arquivo.tmp)
		if g.Match(baseName) {
			return true
		}
		// Casamento no caminho completo
		if g.Match(slashPath) {
			return true
		}
	}

	// Só agora: o arquivo foi manipulado internamente pelo daemon?
	if f.echoSuppressor != nil && f.echoSuppressor.IsSuppressed(cleanPath) {
		return true
	}

	return false
}

// Normalize converte um FileEvent bruto em NormalizedEvent, descartando itens ignorados.
// Retorna (nil, false) caso o evento deva ser suprimido.
func (f *Filter) Normalize(event watcher.FileEvent) (*NormalizedEvent, bool) {
	if f.ShouldIgnore(event.Path) {
		return nil, false
	}

	rel, err := filepath.Rel(f.baseDir, event.Path)
	if err != nil {
		return nil, false
	}

	return &NormalizedEvent{
		WatcherName: event.WatcherName,
		AbsPath:     event.Path,
		RelPath:     filepath.ToSlash(rel),
		Op:          event.Op,
		Timestamp:   event.Timestamp,
		IsDir:       event.IsDir,
	}, true
}

// ProcessStream consome eventos de entrada do watcher, filtra ruídos e despacha para o canal normalizado.
func (f *Filter) ProcessStream(ctx context.Context, in <-chan watcher.FileEvent, out chan<- NormalizedEvent) {
	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-in:
			if !ok {
				return
			}

			normEv, ok := f.Normalize(ev)
			if !ok {
				continue
			}

			select {
			case out <- *normEv:
			case <-ctx.Done():
				return
			}
		}
	}
}

// BaseDir retorna o diretório base canônico associado ao filtro.
func (f *Filter) BaseDir() string {
	return f.baseDir
}

// Patterns retorna a lista de padrões ativos compilados no filtro.
func (f *Filter) Patterns() []string {
	cp := make([]string, len(f.patterns))
	copy(cp, f.patterns)
	return cp
}
