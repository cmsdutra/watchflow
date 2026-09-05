package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/watcher"
)

// TestFilesInNewDirectoryAreNotLost cobre a corrida clássica do inotify: entre
// o mkdir e o registro do watch no novo diretório, arquivos gravados não geram
// evento algum. Sem a varredura de recuperação, um 'git clone' ou 'unzip'
// dentro do vault ficava invisível ao daemon até que os arquivos fossem
// tocados novamente.
func TestFilesInNewDirectoryAreNotLost(t *testing.T) {
	root := t.TempDir()

	w, err := watcher.New("vault", root)
	if err != nil {
		t.Fatalf("falha ao criar watcher: %v", err)
	}
	defer func() { _ = w.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	time.Sleep(150 * time.Millisecond)

	// Cria a árvore inteira "de uma vez", como fazem clone/unzip/cp -r.
	// O conteúdo pode existir antes de o watch do novo diretório ser efetivado.
	nested := filepath.Join(root, "novo", "sub")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(root, "novo", "raiz.md"),
		filepath.Join(nested, "profundo.md"),
	} {
		if err := os.WriteFile(f, []byte("conteúdo"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Coleta tudo o que o watcher reportou
	seen := make(map[string]bool)
	deadline := time.After(3 * time.Second)

collect:
	for {
		select {
		case ev := <-w.Events():
			rel, err := filepath.Rel(w.RootPath(), ev.Path)
			if err == nil {
				seen[filepath.ToSlash(rel)] = true
			}
		case <-deadline:
			break collect
		case <-time.After(600 * time.Millisecond):
			break collect
		}
	}

	for _, want := range []string{"novo/raiz.md", "novo/sub/profundo.md"} {
		if !seen[want] {
			t.Errorf("arquivo '%s' criado em diretório novo nunca gerou evento; eventos vistos: %v", want, keys(seen))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
