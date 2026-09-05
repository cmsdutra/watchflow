package watcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/watchflow/watchflow/internal/logger"
)

// EventOp representa o tipo de operação que originou o evento de sistema de arquivos.
type EventOp string

const (
	// OpCreate indica a criação de um novo arquivo ou diretório.
	OpCreate EventOp = "CREATE"
	// OpWrite indica a escrita/modificação do conteúdo de um arquivo.
	OpWrite EventOp = "WRITE"
	// OpRemove indica a remoção de um arquivo ou diretório.
	OpRemove EventOp = "REMOVE"
	// OpRename indica a renomeação ou movimentação de um item.
	OpRename EventOp = "RENAME"
	// OpChmod indica alteração de permissões ou metadados.
	OpChmod EventOp = "CHMOD"
)

// FileEvent encapsula um evento canônico de sistema de arquivos despachado pelo Watcher.
type FileEvent struct {
	WatcherName string
	Path        string
	Op          EventOp
	Timestamp   time.Time
	IsDir       bool
}

// Watcher gerencia o monitoramento recursivo e contínuo de um diretório raiz.
type Watcher struct {
	name      string
	rootPath  string
	fsWatcher *fsnotify.Watcher
	tree      *DirectoryTree
	events    chan FileEvent
	errors    chan error
	done      chan struct{}
	log       *slog.Logger
	closeOnce sync.Once
}

// New instancia um novo Watcher recursivo para o diretório raiz indicado.
func New(name, rootPath string) (*Watcher, error) {
	if name == "" {
		return nil, fmt.Errorf("nome do watcher não pode ser vazio")
	}

	cleanRoot := filepath.Clean(rootPath)
	canonicalRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("falha ao resolver caminho canônico '%s': %w", cleanRoot, err)
	}

	stat, err := os.Stat(canonicalRoot)
	if err != nil {
		return nil, fmt.Errorf("diretório raiz '%s' não existe: %w", canonicalRoot, err)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("caminho '%s' não é um diretório", canonicalRoot)
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("falha ao inicializar fsnotify: %w", err)
	}

	tree := NewDirectoryTree(fsWatcher)
	if err := tree.WalkAndAdd(canonicalRoot); err != nil {
		_ = fsWatcher.Close()
		return nil, fmt.Errorf("falha ao registrar árvore inicial em '%s': %w", canonicalRoot, err)
	}

	w := &Watcher{
		name:      name,
		rootPath:  canonicalRoot,
		fsWatcher: fsWatcher,
		tree:      tree,
		events:    make(chan FileEvent, 1024),
		errors:    make(chan error, 64),
		done:      make(chan struct{}),
		log:       logger.For("watcher").With(slog.String("watcher", name)),
	}

	return w, nil
}

// Start inicia o loop assíncrono de escuta de eventos com suporte a cancelamento por contexto.
func (w *Watcher) Start(ctx context.Context) {
	go w.eventLoop(ctx)
}

// eventLoop consome eventos brutos do fsnotify, gerencia novas subpastas e despacha FileEvent.
func (w *Watcher) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			_ = w.Close()
			return

		case <-w.done:
			return

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			select {
			case w.errors <- err:
			default:
				// Canal de erros cheio; descarta para não bloquear o loop do
				// watcher, mas registra para não sumir sem rastro.
				w.log.Error("erro do fsnotify descartado (canal de erros saturado)", slog.Any("error", err))
			}

		case ev, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}

			cleanPath := filepath.Clean(ev.Name)
			op := toEventOp(ev.Op)

			// Identifica se o item é um diretório
			isDir := false
			stat, err := os.Stat(cleanPath)
			if err == nil {
				isDir = stat.IsDir()
			}

			// Se uma nova pasta foi criada, registra recursivamente para monitoramento dinâmico
			if ev.Op&fsnotify.Create != 0 && isDir {
				if err := w.tree.WalkAndAdd(cleanPath); err != nil {
					// Ponto cego: alterações dentro deste diretório deixam de
					// gerar eventos e não serão sincronizadas.
					w.log.Error("falha ao registrar novo diretório para monitoramento",
						slog.String("path", cleanPath), slog.Any("error", err))
				} else {
					w.log.Debug("novo diretório sob monitoramento", slog.String("path", cleanPath))
					// Entre o mkdir e o registro do watch existe uma janela em
					// que o kernel não reporta nada. Arquivos gravados nela
					// (git clone, unzip, cp -r) ficariam invisíveis até serem
					// tocados de novo, então são varridos e reemitidos.
					w.emitExistingEntries(ctx, cleanPath)
				}
			}

			// Se uma pasta foi removida, retira do mapa interno
			if ev.Op&fsnotify.Remove != 0 && w.tree.Has(cleanPath) {
				if err := w.tree.RemoveDir(cleanPath); err != nil {
					w.log.Warn("falha ao remover diretório do monitoramento",
						slog.String("path", cleanPath), slog.Any("error", err))
				}
			}

			event := FileEvent{
				WatcherName: w.name,
				Path:        cleanPath,
				Op:          op,
				Timestamp:   time.Now(),
				IsDir:       isDir,
			}

			select {
			case w.events <- event:
			case <-ctx.Done():
				_ = w.Close()
				return
			case <-w.done:
				return
			}
		}
	}
}

// emitExistingEntries varre um diretório recém-registrado e emite eventos
// sintéticos para o conteúdo que já existia, fechando a janela de corrida do
// inotify. Executa na goroutine do eventLoop e aplica contrapressão normalmente.
func (w *Watcher) emitExistingEntries(ctx context.Context, dir string) {
	count := 0

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Diretório removido no meio da varredura: ignora o ramo
		}
		if path == dir {
			return nil
		}

		event := FileEvent{
			WatcherName: w.name,
			Path:        filepath.Clean(path),
			Op:          OpCreate,
			Timestamp:   time.Now(),
			IsDir:       d.IsDir(),
		}

		select {
		case w.events <- event:
			count++
		case <-ctx.Done():
			return fs.SkipAll
		case <-w.done:
			return fs.SkipAll
		}
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		w.log.Warn("varredura de diretório recém-criado interrompida",
			slog.String("path", dir), slog.Any("error", walkErr))
	}

	if count > 0 {
		w.log.Debug("conteúdo preexistente de diretório novo reemitido",
			slog.String("path", dir), slog.Int("entradas", count))
	}
}

// Events retorna o canal somente-leitura de eventos canônicos capturados.
func (w *Watcher) Events() <-chan FileEvent {
	return w.events
}

// Errors retorna o canal somente-leitura de erros emitidos pelo watcher do kernel.
func (w *Watcher) Errors() <-chan error {
	return w.errors
}

// WatchedDirs retorna a lista ordenada de todos os diretórios vigiados atualmente.
func (w *Watcher) WatchedDirs() []string {
	return w.tree.List()
}

// RootPath retorna o caminho absoluto canônico vigiado por esta instância.
func (w *Watcher) RootPath() string {
	return w.rootPath
}

// Name retorna o identificador único do watcher.
func (w *Watcher) Name() string {
	return w.name
}

// Close encerra a escuta do fsnotify e libera descritores de sistema de forma idempotente.
func (w *Watcher) Close() error {
	var err error
	w.closeOnce.Do(func() {
		close(w.done)
		err = w.fsWatcher.Close()
	})
	return err
}

func toEventOp(op fsnotify.Op) EventOp {
	switch {
	case op&fsnotify.Create != 0:
		return OpCreate
	case op&fsnotify.Write != 0:
		return OpWrite
	case op&fsnotify.Remove != 0:
		return OpRemove
	case op&fsnotify.Rename != 0:
		return OpRename
	case op&fsnotify.Chmod != 0:
		return OpChmod
	default:
		return OpWrite
	}
}
