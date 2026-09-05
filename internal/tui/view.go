package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/watchflow/watchflow/internal/ipc"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/queue"
)

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	focusStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("13"))
	selectStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("237"))
	healthyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	pausedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// View compõe o quadro completo do terminal.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	if m.connErr != nil {
		b.WriteString(m.renderDisconnected())
		b.WriteString("\n")
		b.WriteString(m.renderFooter())
		return b.String()
	}

	b.WriteString(m.renderWatchers())
	b.WriteString("\n")
	b.WriteString(m.renderJobs())
	b.WriteString("\n")
	b.WriteString(m.renderAudit())
	b.WriteString("\n")
	b.WriteString(m.renderFooter())

	return b.String()
}

func (m Model) renderHeader() string {
	left := titleStyle.Render(fmt.Sprintf("WatchFlow Monitor %s", m.displayVersion()))

	right := dimStyle.Render("daemon indisponível")
	if m.status != nil {
		right = dimStyle.Render(fmt.Sprintf("PID %d | uptime %s | %d pendente(s) · %d rodando · %d bloqueado(s)",
			m.status.DaemonPID, m.status.Uptime,
			m.status.PendingJobs, m.status.RunningJobs, m.status.BlockedJobs))
	}

	return left + "  " + right
}

func (m Model) displayVersion() string {
	if m.version == "" {
		return ""
	}
	return "v" + strings.TrimPrefix(m.version, "v")
}

func (m Model) renderDisconnected() string {
	var b strings.Builder
	b.WriteString(errorStyle.Render("⚠ Não foi possível falar com o daemon."))
	b.WriteString("\n\n  ")
	b.WriteString(m.connErr.Error())
	b.WriteString("\n\n  ")
	b.WriteString(dimStyle.Render("A TUI segue tentando reconectar. Verifique 'watchflow start' ou o socket configurado."))
	b.WriteString("\n")
	return b.String()
}

func (m Model) renderWatchers() string {
	var b strings.Builder
	b.WriteString(m.panelTitle(PanelWatchers, "COFRES VIGIADOS"))
	b.WriteString("\n")

	if m.status == nil || len(m.status.Watchers) == 0 {
		b.WriteString(dimStyle.Render("  nenhum watcher configurado"))
		b.WriteString("\n")
		return b.String()
	}

	for i, w := range m.status.Watchers {
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}

		line := fmt.Sprintf("%s%s %-18s %-32s %s",
			marker,
			statusBadge(w.Status),
			truncate(w.Name, 18),
			truncate(w.Path, 32),
			dimStyle.Render(lastActivity(w)))

		if i == m.cursor && m.focus == PanelWatchers {
			line = selectStyle.Render(line)
		}

		b.WriteString(line)
		b.WriteString("\n")

		// O erro que interrompeu o watcher é a informação mais acionável da tela
		if w.LastError != "" && (w.Status == string(queue.WatcherConflictHalted) || w.Status == string(queue.WatcherDegraded)) {
			b.WriteString("      ")
			b.WriteString(errorStyle.Render(truncate(flatten(w.LastError), m.contentWidth()-8)))
			b.WriteString("\n")
		}
	}

	return b.String()
}

func (m Model) renderJobs() string {
	var b strings.Builder
	b.WriteString(m.panelTitle(PanelJobs, "FILA PERSISTENTE (SQLITE WAL)"))
	b.WriteString("\n")

	active := activeJobs(m.jobs)
	if len(active) == 0 {
		b.WriteString(dimStyle.Render("  fila vazia — nada pendente"))
		b.WriteString("\n")
		return b.String()
	}

	b.WriteString(dimStyle.Render(fmt.Sprintf("  %-14s %-14s %-10s %-6s %s",
		"PIPELINE", "WATCHER", "STATUS", "TENT.", "DETALHE")))
	b.WriteString("\n")

	for _, j := range active[:min(len(active), m.jobRows())] {
		detail := flatten(j.LastError)
		if detail == "" && j.ScheduledFor != "" {
			detail = "agendado para " + j.ScheduledFor
		}

		b.WriteString(fmt.Sprintf("  %-14s %-14s %s %-6s %s\n",
			truncate(j.PipelineName, 14),
			truncate(j.WatcherID, 14),
			jobStatusStyle(j.Status).Render(fmt.Sprintf("%-10s", j.Status)),
			fmt.Sprintf("%d/%d", j.RetryCount, j.MaxRetries),
			dimStyle.Render(truncate(detail, maxInt(10, m.contentWidth()-52)))))
	}

	return b.String()
}

func (m Model) renderAudit() string {
	var b strings.Builder
	b.WriteString(m.panelTitle(PanelAudit, "AUDITORIA E EVENTOS RECENTES"))
	b.WriteString("\n")

	events := relevantEvents(m.entries, m.auditRows())
	if len(events) == 0 {
		b.WriteString(dimStyle.Render("  nenhum evento registrado ainda"))
		b.WriteString("\n")
		return b.String()
	}

	for _, e := range events {
		b.WriteString("  ")
		b.WriteString(dimStyle.Render(e.Time.Format("15:04:05")))
		b.WriteString(" ")
		b.WriteString(levelStyle(e.Level).Render(fmt.Sprintf("%-11s", truncate(e.Component, 11))))
		b.WriteString(" ")
		b.WriteString(truncate(eventLine(e), maxInt(20, m.contentWidth()-24)))
		b.WriteString("\n")
	}

	return b.String()
}

// eventLine compõe a descrição de um evento, anexando os detalhes que mais
// importam quando presentes (step, duração, contagem de arquivos).
func eventLine(e logger.Entry) string {
	parts := []string{e.Message}

	if step := e.Attr("step"); step != "" {
		parts = append(parts, "— "+step)
	}
	if args := e.Attr("args"); args != "" {
		parts = append(parts, "— git "+args)
	}
	if files := e.Attr("arquivos"); files != "" {
		parts = append(parts, "("+files+" arquivo(s))")
	}
	if d := e.Attr("duracao"); d != "" {
		parts = append(parts, "("+d+")")
	} else if d := e.Attr("elapsed"); d != "" {
		parts = append(parts, "("+d+")")
	}
	if errText := e.Attr("error"); errText != "" {
		parts = append(parts, "— "+flatten(errText))
	}

	return strings.Join(parts, " ")
}

// relevantEvents seleciona os eventos mais recentes que valem exibição,
// descartando o ruído de nível debug que não descreve uma ação do pipeline.
func relevantEvents(entries []logger.Entry, limit int) []logger.Entry {
	if limit <= 0 {
		return nil
	}

	filtered := make([]logger.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Level == "DEBUG" && e.Component != "git" && e.Component != "pipeline" {
			continue
		}
		filtered = append(filtered, e)
	}

	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered
}

func (m Model) renderFooter() string {
	var b strings.Builder

	if m.feedback != "" && time.Now().Before(m.feedbackTill) {
		style := dimStyle
		if m.feedbackKind == feedbackError {
			style = errorStyle
		}
		b.WriteString("  ")
		b.WriteString(style.Render(truncate(flatten(m.feedback), m.contentWidth()-4)))
		b.WriteString("\n")
	}

	keys := []string{
		"[s] sincronizar",
		"[p] pausar/retomar",
		"[r] atualizar",
		"[Tab] foco",
		"[↑↓] navegar",
		"[q] sair",
	}
	b.WriteString("  ")
	b.WriteString(dimStyle.Render(strings.Join(keys, "  │  ")))

	return b.String()
}

func (m Model) panelTitle(p Panel, label string) string {
	if m.focus == p {
		return focusStyle.Render("▌ " + label)
	}
	return headerStyle.Render("  " + label)
}

// contentWidth é a largura útil considerando as margens laterais.
func (m Model) contentWidth() int {
	if m.width <= 0 {
		return 100
	}
	return m.width
}

// jobRows e auditRows repartem a altura restante entre os dois painéis de
// lista, garantindo um mínimo utilizável mesmo em terminais baixos.
func (m Model) jobRows() int {
	return maxInt(3, (m.availableRows())/3)
}

func (m Model) auditRows() int {
	return maxInt(3, m.availableRows()-m.jobRows())
}

// availableRows desconta cabeçalho, títulos de painel, rodapé e a lista de
// watchers, que tem altura variável.
func (m Model) availableRows() int {
	const chrome = 12
	rows := m.height - chrome - m.watcherCount()
	return maxInt(6, rows)
}

// activeJobs prioriza o que exige atenção: bloqueados e em execução primeiro,
// depois pendentes. Jobs concluídos não ocupam espaço na fila.
func activeJobs(jobs []ipc.JobDTO) []ipc.JobDTO {
	var blocked, running, pending []ipc.JobDTO

	for _, j := range jobs {
		switch j.Status {
		case string(queue.StatusBlocked):
			blocked = append(blocked, j)
		case string(queue.StatusRunning):
			running = append(running, j)
		case string(queue.StatusPending):
			pending = append(pending, j)
		}
	}

	out := make([]ipc.JobDTO, 0, len(blocked)+len(running)+len(pending))
	out = append(out, blocked...)
	out = append(out, running...)
	return append(out, pending...)
}

func statusBadge(status string) string {
	switch status {
	case string(queue.WatcherHealthy):
		return healthyStyle.Render("● HEALTHY        ")
	case string(queue.WatcherPaused):
		return pausedStyle.Render("○ PAUSED         ")
	case string(queue.WatcherDegraded):
		return warnStyle.Render("◐ DEGRADED       ")
	case string(queue.WatcherConflictHalted):
		return errorStyle.Render("✖ CONFLICT_HALTED")
	default:
		return dimStyle.Render(fmt.Sprintf("%-17s", status))
	}
}

func jobStatusStyle(status string) lipgloss.Style {
	switch status {
	case string(queue.StatusBlocked), string(queue.StatusFailed):
		return errorStyle
	case string(queue.StatusRunning):
		return healthyStyle
	case string(queue.StatusPending):
		return warnStyle
	default:
		return dimStyle
	}
}

func levelStyle(level string) lipgloss.Style {
	switch level {
	case "ERROR":
		return errorStyle
	case "WARN":
		return warnStyle
	default:
		return dimStyle
	}
}

// lastActivity resume a atividade mais recente do watcher em uma coluna.
func lastActivity(w ipc.WatcherStatusDTO) string {
	if w.LastSuccessSyncAt != "" {
		return "último sync: " + w.LastSuccessSyncAt
	}
	if w.LastEventAt != "" {
		return "último evento: " + w.LastEventAt
	}
	return "sem atividade"
}

func flatten(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

func truncate(s string, max int) string {
	if max <= 1 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
