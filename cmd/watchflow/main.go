package main

import (
	"fmt"
	"os"

	"github.com/watchflow/watchflow/internal/logger"
)

func main() {
	// Comandos de CLI não devem poluir stdout/stderr com o log do daemon.
	// O comando 'start' reconfigura o logger a partir da configuração.
	logger.Discard()

	if err := Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Erro: %v\n", err)
		os.Exit(1)
	}
}
