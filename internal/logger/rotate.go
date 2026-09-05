package logger

import (
	"fmt"
	"os"
	"sync"
)

// defaultMaxSizeBytes é o tamanho a partir do qual o arquivo de log é rotacionado.
const defaultMaxSizeBytes int64 = 8 << 20 // 8 MiB

// rotatingFile é um io.WriteCloser que mantém o log limitado a dois arquivos
// (<nome> e <nome>.1). Evita que um daemon de execução contínua encha o disco,
// sem introduzir dependência externa de rotação.
type rotatingFile struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	size    int64
	file    *os.File
}

func newRotatingFile(path string, maxSize int64) (*rotatingFile, error) {
	if maxSize <= 0 {
		maxSize = defaultMaxSizeBytes
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("falha ao abrir arquivo de log '%s': %w", path, err)
	}

	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}

	return &rotatingFile{path: path, maxSize: maxSize, size: size, file: f}, nil
}

func (w *rotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			// Rotação falhou: reporta em stderr (não é possível usar o logger
			// aqui sem recursão) e segue escrevendo, em vez de perder a linha.
			fmt.Fprintf(os.Stderr, "watchflow: falha ao rotacionar log '%s': %v\n", w.path, err)
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingFile) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}

	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// Reabre o arquivo original para não interromper o fluxo de logs
		f, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if openErr != nil {
			return openErr
		}
		w.file = f
		return err
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}

	w.file = f
	w.size = 0
	return nil
}

func (w *rotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
