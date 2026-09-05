package notify

import (
	"os"
	"testing"

	"github.com/watchflow/watchflow/internal/logger"
)

// TestMain silencia o logging estruturado durante os testes.
func TestMain(m *testing.M) {
	logger.Discard()
	os.Exit(m.Run())
}
