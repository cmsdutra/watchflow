package providers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Erros sentinela comuns a provedores de ações.
var (
	ErrConflict         = errors.New("conflito de merge detectado")
	ErrActionNotFound   = errors.New("ação não registrada no catálogo")
	ErrValidationFailed = errors.New("validação dos parâmetros da ação falhou")
)

// StepContext fornece os dados de contexto necessários para execução de uma ação atômica.
type StepContext struct {
	Context      context.Context
	WatcherName  string
	BasePath     string
	ChangedFiles []string
	StepParams   map[string]interface{}
	LastOutput   string
	Timestamp    time.Time
}

// StepResult encapsula o resultado estruturado da execução de um step.
type StepResult struct {
	Success       bool
	Skipped       bool
	TransientErr  bool
	ConflictErr   bool
	ErrorMessage  string
	AffectedPaths []string
	Output        string
}

// ActionProvider define o contrato que todo provider e ação de pipeline deve implementar.
type ActionProvider interface {
	Name() string
	Validate(params map[string]interface{}) error
	Execute(ctx *StepContext) (*StepResult, error)
}

// Registry gerencia o catálogo thread-safe de ações registradas no sistema.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]ActionProvider
}

// NewRegistry cria uma nova instância isolada de registro de ações.
func NewRegistry() *Registry {
	return &Registry{
		actions: make(map[string]ActionProvider),
	}
}

// DefaultRegistry é o catálogo global padrão de provedores e ações do WatchFlow.
var DefaultRegistry = NewRegistry()

// Register adiciona uma ação ao catálogo do registro.
func (r *Registry) Register(provider ActionProvider) error {
	if provider == nil {
		return fmt.Errorf("provider não pode ser nulo")
	}

	name := provider.Name()
	if name == "" {
		return fmt.Errorf("nome do provider não pode ser vazio")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.actions[name]; exists {
		return fmt.Errorf("provider com o nome '%s' já registrado", name)
	}

	r.actions[name] = provider
	return nil
}

// Get busca uma ação pelo nome no catálogo.
func (r *Registry) Get(name string) (ActionProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.actions[name]
	return p, ok
}

// List retorna a lista ordenada de nomes de ações cadastradas no catálogo.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.actions))
	for name := range r.actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Register registra uma ação no catálogo global DefaultRegistry.
func Register(provider ActionProvider) error {
	return DefaultRegistry.Register(provider)
}

// Get busca uma ação no catálogo global DefaultRegistry.
func Get(name string) (ActionProvider, bool) {
	return DefaultRegistry.Get(name)
}
