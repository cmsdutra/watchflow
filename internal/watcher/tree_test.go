package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestDirectoryTreeOperations(t *testing.T) {
	tempDir := t.TempDir()

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("falha ao criar fsnotify watcher: %v", err)
	}
	defer func() { _ = fsWatcher.Close() }()

	tree := NewDirectoryTree(fsWatcher)

	dir1 := filepath.Join(tempDir, "dir1")
	dir2 := filepath.Join(tempDir, "dir2")
	if err := os.Mkdir(dir1, 0755); err != nil {
		t.Fatalf("falha ao criar dir1: %v", err)
	}
	if err := os.Mkdir(dir2, 0755); err != nil {
		t.Fatalf("falha ao criar dir2: %v", err)
	}

	// 1. AddDir
	if err := tree.AddDir(dir1); err != nil {
		t.Fatalf("falha no AddDir: %v", err)
	}
	// Testar idempotência (adicionar mesmo dir não deve dar erro)
	if err := tree.AddDir(dir1); err != nil {
		t.Fatalf("AddDir repetido deve ser no-op: %v", err)
	}

	if !tree.Has(dir1) {
		t.Errorf("esperava Has(dir1) == true")
	}
	if tree.Has(dir2) {
		t.Errorf("esperava Has(dir2) == false")
	}

	if count := tree.Count(); count != 1 {
		t.Errorf("esperava Count == 1, obteve %d", count)
	}

	// 2. RemoveDir
	if err := tree.RemoveDir(dir1); err != nil {
		t.Fatalf("falha no RemoveDir: %v", err)
	}
	if tree.Has(dir1) {
		t.Errorf("esperava Has(dir1) == false após remoção")
	}
	// Remover diretório inexistente não deve dar erro
	if err := tree.RemoveDir(dir1); err != nil {
		t.Errorf("RemoveDir repetido deve ser no-op: %v", err)
	}

	// 3. WalkAndAdd com subpastas aninhadas
	nested := filepath.Join(dir2, "nest1", "nest2")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("falha ao criar estrutura aninhada: %v", err)
	}

	if err := tree.WalkAndAdd(dir2); err != nil {
		t.Fatalf("falha no WalkAndAdd: %v", err)
	}

	list := tree.List()
	if len(list) != 3 {
		t.Errorf("esperava 3 diretórios na lista, obteve %d: %v", len(list), list)
	}
}

func TestToEventOp(t *testing.T) {
	testCases := []struct {
		in  fsnotify.Op
		out EventOp
	}{
		{fsnotify.Create, OpCreate},
		{fsnotify.Write, OpWrite},
		{fsnotify.Remove, OpRemove},
		{fsnotify.Rename, OpRename},
		{fsnotify.Chmod, OpChmod},
		{0, OpWrite}, // Default fallback
	}

	for _, tc := range testCases {
		res := toEventOp(tc.in)
		if res != tc.out {
			t.Errorf("para op %v esperava %s, obteve %s", tc.in, tc.out, res)
		}
	}
}
