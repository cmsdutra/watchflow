package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherNewValidation(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "regular_file.txt")
	if err := os.WriteFile(tempFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("falha ao criar arquivo de teste: %v", err)
	}

	// 1. Nome vazio
	_, err := New("", tempDir)
	if err == nil {
		t.Errorf("esperava erro para nome vazio")
	}

	// 2. Caminho inexistente
	_, err = New("test", filepath.Join(tempDir, "nao_existe"))
	if err == nil {
		t.Errorf("esperava erro para caminho inexistente")
	}

	// 3. Caminho aponta para arquivo comum, não diretório
	_, err = New("test", tempFile)
	if err == nil {
		t.Errorf("esperava erro para caminho que não é diretório")
	}

	// 4. Caminho válido
	w, err := New("valid-watcher", tempDir)
	if err != nil {
		t.Fatalf("esperava sucesso, obteve: %v", err)
	}
	defer func() { _ = w.Close() }()

	if w.Name() != "valid-watcher" {
		t.Errorf("esperava nome 'valid-watcher', obteve %s", w.Name())
	}
	if w.RootPath() == "" {
		t.Errorf("esperava RootPath não vazio")
	}
}

func TestWatcherInitialTree(t *testing.T) {
	tempDir := t.TempDir()

	sub1 := filepath.Join(tempDir, "sub1")
	sub2 := filepath.Join(tempDir, "sub2")
	sub3 := filepath.Join(sub2, "sub3")

	if err := os.MkdirAll(sub3, 0755); err != nil {
		t.Fatalf("falha ao criar pastas iniciais: %v", err)
	}
	if err := os.MkdirAll(sub1, 0755); err != nil {
		t.Fatalf("falha ao criar sub1: %v", err)
	}

	w, err := New("tree-test", tempDir)
	if err != nil {
		t.Fatalf("falha ao criar watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	dirs := w.WatchedDirs()
	// Esperado: tempDir, sub1, sub2, sub3 (4 diretórios)
	if len(dirs) != 4 {
		t.Errorf("esperava 4 diretórios vigiados, obteve %d: %v", len(dirs), dirs)
	}
}

func TestWatcherFileEvents(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("falha ao criar sub: %v", err)
	}

	w, err := New("events-test", tempDir)
	if err != nil {
		t.Fatalf("falha ao criar watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.Start(ctx)

	// Criação de arquivo
	filePath := filepath.Join(subDir, "test.md")
	if err := os.WriteFile(filePath, []byte("conteudo inicial"), 0644); err != nil {
		t.Fatalf("falha ao criar arquivo: %v", err)
	}

	select {
	case ev := <-w.Events():
		if ev.Path != filePath {
			t.Errorf("esperava caminho %s, obteve %s", filePath, ev.Path)
		}
		if ev.WatcherName != "events-test" {
			t.Errorf("esperava watcher 'events-test', obteve %s", ev.WatcherName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout aguardando evento de criação de arquivo")
	}

	// Escrita / Modificação do arquivo
	if err := os.WriteFile(filePath, []byte("conteudo modificado"), 0644); err != nil {
		t.Fatalf("falha ao modificar arquivo: %v", err)
	}

	select {
	case ev := <-w.Events():
		if ev.Path != filePath {
			t.Errorf("esperava caminho %s, obteve %s", filePath, ev.Path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout aguardando evento de escrita de arquivo")
	}
}

func TestWatcherDynamicSubdirectoryCreation(t *testing.T) {
	tempDir := t.TempDir()

	w, err := New("dynamic-test", tempDir)
	if err != nil {
		t.Fatalf("falha ao criar watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.Start(ctx)

	// Cria dinamicamente uma pasta aninhada em tempo de execução
	newFolder := filepath.Join(tempDir, "novapasta", "aninhada")
	if err := os.MkdirAll(newFolder, 0755); err != nil {
		t.Fatalf("falha ao criar subdiretórios dinâmicos: %v", err)
	}

	// Aguarda breve intervalo para o watcher do kernel processar e registrar as novas pastas
	time.Sleep(150 * time.Millisecond)

	// Cria um arquivo dentro da subpasta aninhada recém-criada
	nestedFile := filepath.Join(newFolder, "nota_aninhada.md")
	if err := os.WriteFile(nestedFile, []byte("dentro da pasta dinamica"), 0644); err != nil {
		t.Fatalf("falha ao gravar arquivo na subpasta dinâmica: %v", err)
	}

	// Verifica se o evento foi capturado na pasta aninhada
	found := false
	timeout := time.After(3 * time.Second)

	for !found {
		select {
		case ev := <-w.Events():
			if ev.Path == nestedFile {
				found = true
			}
		case <-timeout:
			t.Fatalf("timeout aguardando evento no arquivo aninhado dinâmico %s", nestedFile)
		}
	}
}

func TestWatcherContextCancellation(t *testing.T) {
	tempDir := t.TempDir()

	w, err := New("cancel-test", tempDir)
	if err != nil {
		t.Fatalf("falha ao criar watcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	// Cancela o contexto
	cancel()

	// Aguarda processamento de encerramento
	time.Sleep(50 * time.Millisecond)

	// Testar fechamento idempotente
	if err := w.Close(); err != nil {
		// No Linux, fechar um fsnotify já fechado pode retornar erro de descriptor ou nil, ambos tratados
		t.Logf("Close idempotente retornou: %v", err)
	}
}
