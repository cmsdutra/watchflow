package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/watchflow/watchflow/internal/logger"
)

// LogTail lê a auditoria a partir do arquivo de log estruturado do daemon.
type LogTail struct {
	Path string
}

// Recent devolve as entradas mais recentes do log.
func (l LogTail) Recent(limit int) ([]logger.Entry, error) {
	return logger.Tail(l.Path, limit)
}

// Run inicia o loop da interface até o usuário sair.
//
// A TUI usa a tela alternativa do terminal: ao encerrar, o conteúdo anterior do
// shell é restaurado intacto.
func Run(opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("cliente IPC não configurado")
	}

	program := tea.NewProgram(New(opts), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("falha ao executar a interface: %w", err)
	}
	return nil
}
