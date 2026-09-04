package normalizer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/watcher"
)

func TestFilterShouldIgnore(t *testing.T) {
	baseDir := "/home/caio/Chaos"

	userPatterns := []string{
		".obsidian/workspace*",
		"**/*.bak",
		"private/**",
	}

	filter, err := NewFilter(baseDir, userPatterns)
	if err != nil {
		t.Fatalf("falha ao instanciar filtro: %v", err)
	}

	testCases := []struct {
		relPath      string
		shouldIgnore bool
		description  string
	}{
		// Casos que DEVEM ser ignorados
		{".git", true, "pasta raiz .git"},
		{".git/HEAD", true, "arquivo HEAD do git"},
		{".git/index", true, "arquivo index do git"},
		{".git/index.lock", true, "trava temporária do git"},
		{".git/objects/4b/825dc6", true, "objeto do git"},
		{"subpasta/.git/config", true, "submódulo ou git interno"},
		{"nota.tmp", true, "arquivo temporário .tmp na raiz"},
		{"pasta/sub/arquivo.tmp", true, "arquivo .tmp aninhado"},
		{"rascunho.md~", true, "arquivo de backup do editor com til"},
		{"sub/teste.swp", true, "swap file de editor"},
		{".DS_Store", true, "arquivo de metadados macOS na raiz"},
		{"sub/.DS_Store", true, "arquivo de metadados macOS aninhado"},
		{".obsidian/cache/graph.json", true, "cache interno do Obsidian"},
		{".obsidian/cache/tokens.bin", true, "cache de tokens do Obsidian"},
		{".obsidian/workspace.json", true, "workspace do Obsidian (regra de usuário)"},
		{".obsidian/workspace-mobile.json", true, "workspace mobile do Obsidian"},
		{"nota.bak", true, "arquivo de backup .bak"},
		{"pasta/arquivo.bak", true, "arquivo .bak aninhado"},
		{"private/segredo.txt", true, "pasta privada (regra de usuário)"},
		{"private/sub/dados.csv", true, "conteúdo aninhado em pasta privada"},
		{".tmp/session.lock", true, "pasta temporária oculta .tmp"},

		// Casos que NÃO DEVEM ser ignorados (devem passar com sucesso)
		{"Inbox/nota.md", false, "nota normal do Obsidian"},
		{"Projetos/WatchFlow/README.md", false, "arquivo Markdown de documentação"},
		{"Diario/2026-09-04.md", false, "nota de diário"},
		{"anexos/diagrama.png", false, "imagem de anexo"},
		{"anexos/documento.pdf", false, "arquivo PDF legítimo"},
		{"codigo/main.go", false, "código fonte Go"},
		{"dados.json", false, "arquivo JSON normal fora de pastas ignoradas"},
		{"pasta_sem_ponto/sub/nota.txt", false, "arquivo de texto em pasta comum"},
		{"minhas_notas.git.md", false, "arquivo com .git no meio do nome mas não diretório"},
		{"temporario.md", false, "arquivo com 'temporario' no nome mas extensão normal"},
	}

	for _, tc := range testCases {
		absPath := filepath.Join(baseDir, tc.relPath)
		got := filter.ShouldIgnore(absPath)
		if got != tc.shouldIgnore {
			t.Errorf("Path: '%s' (%s) -> esperava ShouldIgnore=%v, obteve=%v",
				tc.relPath, tc.description, tc.shouldIgnore, got)
		}
	}
}

func TestPathTraversalProtection(t *testing.T) {
	baseDir := "/home/caio/Chaos"
	filter, err := NewFilter(baseDir, nil)
	if err != nil {
		t.Fatalf("falha ao instanciar filtro: %v", err)
	}

	// Caminhos fora do diretório raiz devem ser sumariamente descartados
	traversalPaths := []string{
		"/etc/passwd",
		"/home/caio/OutroDiretorio/nota.md",
		"/home/caio/Chaos/../OutroDiretorio/nota.md",
	}

	for _, p := range traversalPaths {
		if !filter.ShouldIgnore(p) {
			t.Errorf("caminho fora da raiz '%s' deveria ser ignorado por segurança", p)
		}
	}
}

func TestFilterNormalize(t *testing.T) {
	baseDir := "/home/caio/Chaos"
	filter, err := NewFilter(baseDir, nil)
	if err != nil {
		t.Fatalf("falha ao instanciar filtro: %v", err)
	}

	validEvent := watcher.FileEvent{
		WatcherName: "personal-vault",
		Path:        filepath.Join(baseDir, "Inbox", "ideia.md"),
		Op:          watcher.OpWrite,
		Timestamp:   time.Now(),
		IsDir:       false,
	}

	normEv, ok := filter.Normalize(validEvent)
	if !ok || normEv == nil {
		t.Fatalf("esperava normalização com sucesso para evento válido")
	}

	if normEv.RelPath != "Inbox/ideia.md" {
		t.Errorf("esperava RelPath 'Inbox/ideia.md', obteve '%s'", normEv.RelPath)
	}
	if normEv.Op != watcher.OpWrite {
		t.Errorf("esperava Op OpWrite, obteve '%s'", normEv.Op)
	}
	if normEv.WatcherName != "personal-vault" {
		t.Errorf("esperava WatcherName 'personal-vault', obteve '%s'", normEv.WatcherName)
	}

	// Evento em arquivo ignorado deve retornar (nil, false)
	ignoredEvent := watcher.FileEvent{
		WatcherName: "personal-vault",
		Path:        filepath.Join(baseDir, ".git", "index.lock"),
		Op:          watcher.OpCreate,
		Timestamp:   time.Now(),
		IsDir:       false,
	}

	if _, ok := filter.Normalize(ignoredEvent); ok {
		t.Errorf("evento em .git/index.lock deveria ser descartado pelo Normalize")
	}
}

func TestFilterProcessStream(t *testing.T) {
	baseDir := "/home/caio/Chaos"
	filter, err := NewFilter(baseDir, nil)
	if err != nil {
		t.Fatalf("falha ao instanciar filtro: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inChan := make(chan watcher.FileEvent, 10)
	outChan := make(chan NormalizedEvent, 10)

	go filter.ProcessStream(ctx, inChan, outChan)

	now := time.Now()
	// Envia evento ruído (ignorado)
	inChan <- watcher.FileEvent{WatcherName: "w", Path: filepath.Join(baseDir, ".git", "HEAD"), Op: watcher.OpWrite, Timestamp: now}
	// Envia evento válido
	inChan <- watcher.FileEvent{WatcherName: "w", Path: filepath.Join(baseDir, "nota.md"), Op: watcher.OpCreate, Timestamp: now}
	// Envia outro ruído
	inChan <- watcher.FileEvent{WatcherName: "w", Path: filepath.Join(baseDir, "cache.tmp"), Op: watcher.OpWrite, Timestamp: now}

	select {
	case ev := <-outChan:
		if ev.RelPath != "nota.md" {
			t.Errorf("esperava 'nota.md', obteve: %s", ev.RelPath)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout aguardando evento no outChan")
	}

	// Garante que nenhum outro evento vazou para outChan
	select {
	case unexpected := <-outChan:
		t.Fatalf("evento inesperado recebido: %v", unexpected)
	case <-time.After(100 * time.Millisecond):
		// Sucesso: fila vazia
	}
}

func TestInvalidGlobPattern(t *testing.T) {
	_, err := NewFilter("/tmp", []string{"[invalid-glob"})
	if err == nil {
		t.Errorf("esperava erro ao compilar padrão glob com colchete não fechado")
	}
}

func TestFilterGetters(t *testing.T) {
	baseDir := "/home/caio/Chaos"
	filter, err := NewFilter(baseDir, []string{"custom/*"})
	if err != nil {
		t.Fatalf("falha ao instanciar filtro: %v", err)
	}

	if filter.BaseDir() == "" {
		t.Errorf("esperava BaseDir não vazio")
	}
	if len(filter.Patterns()) == 0 {
		t.Errorf("esperava lista de Patterns não vazia")
	}
}

func BenchmarkShouldIgnore(t *testing.B) {
	baseDir := "/home/caio/Chaos"
	filter, _ := NewFilter(baseDir, []string{".obsidian/workspace*", "*.bak"})
	testPath := filepath.Join(baseDir, "Sub", "Diretorio", "Nota.md")

	t.ResetTimer()
	for i := 0; i < t.N; i++ {
		filter.ShouldIgnore(testPath)
	}
}
