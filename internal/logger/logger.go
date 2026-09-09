// Package logger fornece o logging estruturado JSON do WatchFlow, com
// supressão central de segredos (ver README, "Invariantes de engenharia") e rotação por tamanho.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// LogFileName é o nome do arquivo de log JSON gravado no diretório de estado.
const LogFileName = "watchflow.log"

// Options descreve a configuração do subsistema de logs.
type Options struct {
	// Level aceita "debug", "info", "warn" ou "error". Vazio equivale a "info".
	Level string
	// Dir é o diretório de estado onde o log JSON é gravado. Vazio desabilita
	// a saída em arquivo (útil em testes e no comando 'config validate').
	Dir string
	// Stderr também emite as linhas em stderr, de onde o journald as coleta
	// quando o daemon roda sob systemd --user.
	Stderr bool
	// MaxSizeBytes define o limite de rotação. Zero usa o padrão de 8 MiB.
	MaxSizeBytes int64
	// AddSource inclui arquivo e linha de origem em cada registro.
	AddSource bool
}

// ParseLevel converte o valor textual de daemon.log_level em slog.Level.
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("nível de log '%s' inválido; use: debug, info, warn, error", level)
	}
}

// Setup constrói o logger estruturado e o instala como padrão global via
// slog.SetDefault. Retorna o io.Closer do arquivo de log (nil quando não há
// saída em arquivo), que deve ser fechado no encerramento do daemon.
func Setup(opts Options) (io.Closer, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	var (
		writers []io.Writer
		closer  io.Closer
	)

	if opts.Stderr {
		writers = append(writers, os.Stderr)
	}

	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0700); err != nil {
			return nil, fmt.Errorf("falha ao criar diretório de logs '%s': %w", opts.Dir, err)
		}

		rf, err := newRotatingFile(filepath.Join(opts.Dir, LogFileName), opts.MaxSizeBytes)
		if err != nil {
			return nil, err
		}

		writers = append(writers, rf)
		closer = rf
	}

	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}

	var out io.Writer = writers[0]
	if len(writers) > 1 {
		out = io.MultiWriter(writers...)
	}

	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:       level,
		AddSource:   opts.AddSource,
		ReplaceAttr: readableDurations,
	})

	slog.SetDefault(slog.New(redactHandler{inner: handler}))

	return closer, nil
}

// readableDurations grava durações como "300ms" em vez do inteiro de
// nanossegundos, que aparecia em notação científica ("3e+08") ao ser relido.
func readableDurations(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindDuration {
		return slog.String(a.Key, a.Value.Duration().String())
	}
	return a
}

// For devolve um logger rotulado com o componente de origem, permitindo filtrar
// o log JSON por subsistema (ex.: jq 'select(.component=="watcher")').
func For(component string) *slog.Logger {
	return slog.Default().With(slog.String("component", component))
}

// Discard instala um logger silencioso. Usado por testes que não querem ruído.
func Discard() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
}
