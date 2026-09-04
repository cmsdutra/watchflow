package providers_test

import (
	"context"
	"testing"

	"github.com/watchflow/watchflow/internal/providers"
)

type dummyProvider struct {
	name string
}

func (d *dummyProvider) Name() string {
	return d.name
}

func (d *dummyProvider) Validate(params map[string]interface{}) error {
	return nil
}

func (d *dummyProvider) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	return &providers.StepResult{Success: true}, nil
}

func TestRegistry_Operations(t *testing.T) {
	reg := providers.NewRegistry()

	// 1. Registro nulo
	if err := reg.Register(nil); err == nil {
		t.Errorf("esperava erro ao registrar provider nulo")
	}

	// 2. Registro com nome vazio
	emptyName := &dummyProvider{name: ""}
	if err := reg.Register(emptyName); err == nil {
		t.Errorf("esperava erro ao registrar provider com nome vazio")
	}

	// 3. Registro com sucesso
	p1 := &dummyProvider{name: "git.add"}
	p2 := &dummyProvider{name: "git.commit"}
	p3 := &dummyProvider{name: "git.check_locks"}

	if err := reg.Register(p1); err != nil {
		t.Fatalf("falha ao registrar p1: %v", err)
	}
	if err := reg.Register(p2); err != nil {
		t.Fatalf("falha ao registrar p2: %v", err)
	}
	if err := reg.Register(p3); err != nil {
		t.Fatalf("falha ao registrar p3: %v", err)
	}

	// 4. Registro duplicado
	if err := reg.Register(p1); err == nil {
		t.Errorf("esperava erro ao registrar provider duplicado")
	}

	// 5. Busca existente e inexistente
	found, ok := reg.Get("git.add")
	if !ok || found.Name() != "git.add" {
		t.Errorf("esperava encontrar git.add")
	}

	_, ok = reg.Get("non.existent")
	if ok {
		t.Errorf("esperava não encontrar non.existent")
	}

	// 6. Listagem ordenada
	names := reg.List()
	expected := []string{"git.add", "git.check_locks", "git.commit"}
	if len(names) != len(expected) {
		t.Fatalf("esperava %d nomes, obteve %d", len(expected), len(names))
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("posição %d: esperava %s, obteve %s", i, expected[i], name)
		}
	}
}

func TestDefaultRegistry(t *testing.T) {
	p := &dummyProvider{name: "default.test.action"}
	if err := providers.Register(p); err != nil {
		t.Fatalf("falha ao registrar no DefaultRegistry: %v", err)
	}

	found, ok := providers.Get("default.test.action")
	if !ok || found == nil {
		t.Errorf("esperava encontrar no DefaultRegistry")
	}

	res, err := found.Execute(&providers.StepContext{Context: context.Background()})
	if err != nil || !res.Success {
		t.Errorf("esperava sucesso na execução do dummy")
	}
}
