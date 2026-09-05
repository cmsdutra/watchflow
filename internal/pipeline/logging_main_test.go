package pipeline_test

import (
	"os"
	"testing"

	"github.com/watchflow/watchflow/internal/logger"
)

// TestMain silencia o logging estruturado durante os testes, mantendo a saída
// do 'go test' legível. Os testes do próprio pacote logger configuram sinks
// dedicados e não passam por aqui.
func TestMain(m *testing.M) {
	logger.Discard()
	os.Exit(m.Run())
}
