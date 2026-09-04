package watcher

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// DirectoryTree gerencia o conjunto de diretórios vigiados recursivamente em memória.
type DirectoryTree struct {
	mu        sync.RWMutex
	dirs      map[string]bool
	fsWatcher *fsnotify.Watcher
}

// NewDirectoryTree instancia um novo gerenciador de árvore de diretórios.
func NewDirectoryTree(fsWatcher *fsnotify.Watcher) *DirectoryTree {
	return &DirectoryTree{
		dirs:      make(map[string]bool),
		fsWatcher: fsWatcher,
	}
}

// AddDir adiciona um único diretório ao monitor fsnotify e ao mapa interno.
func (dt *DirectoryTree) AddDir(path string) error {
	cleanPath := filepath.Clean(path)

	dt.mu.Lock()
	defer dt.mu.Unlock()

	if dt.dirs[cleanPath] {
		return nil
	}

	if err := dt.fsWatcher.Add(cleanPath); err != nil {
		return fmt.Errorf("falha ao adicionar diretório '%s' ao fsnotify: %w", cleanPath, err)
	}

	dt.dirs[cleanPath] = true
	return nil
}

// RemoveDir remove o diretório do monitor fsnotify e do mapa interno.
func (dt *DirectoryTree) RemoveDir(path string) error {
	cleanPath := filepath.Clean(path)

	dt.mu.Lock()
	defer dt.mu.Unlock()

	if !dt.dirs[cleanPath] {
		return nil
	}

	delete(dt.dirs, cleanPath)
	// O inotify do Linux frequentemente remove descritores de diretórios deletados automaticamente;
	// ignoramos erro caso o watcher já tenha sido descartado pelo kernel.
	_ = dt.fsWatcher.Remove(cleanPath)
	return nil
}

// WalkAndAdd percorre recursivamente a partir de root e adiciona todas as subpastas encontradas.
func (dt *DirectoryTree) WalkAndAdd(root string) error {
	cleanRoot := filepath.Clean(root)

	return filepath.WalkDir(cleanRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Se o diretório foi deletado durante a varredura, ignora
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		if d.IsDir() {
			if addErr := dt.AddDir(path); addErr != nil {
				// Se a pasta deixou de existir entre a leitura e o watch, ignora
				if os.IsNotExist(addErr) {
					return nil
				}
				return addErr
			}
		}
		return nil
	})
}

// Has verifica se um diretório específico já está sendo vigiado.
func (dt *DirectoryTree) Has(path string) bool {
	dt.mu.RLock()
	defer dt.mu.RUnlock()
	return dt.dirs[filepath.Clean(path)]
}

// List retorna uma lista ordenada de todos os diretórios monitorados.
func (dt *DirectoryTree) List() []string {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	result := make([]string, 0, len(dt.dirs))
	for dir := range dt.dirs {
		result = append(result, dir)
	}
	sort.Strings(result)
	return result
}

// Count retorna o número total de diretórios vigiados.
func (dt *DirectoryTree) Count() int {
	dt.mu.RLock()
	defer dt.mu.RUnlock()
	return len(dt.dirs)
}
