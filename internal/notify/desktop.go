package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// notifySendBinary é o utilitário padrão de notificação desktop no Linux.
const notifySendBinary = "notify-send"

// desktopNotifier entrega alertas via notify-send (libnotify).
type desktopNotifier struct {
	binary string

	// run é injetável para permitir teste sem sessão gráfica.
	run func(ctx context.Context, name string, args ...string) error
}

func newDesktopNotifier() (*desktopNotifier, error) {
	path, err := exec.LookPath(notifySendBinary)
	if err != nil {
		return nil, fmt.Errorf("'%s' não encontrado no PATH: %w", notifySendBinary, err)
	}

	return &desktopNotifier{binary: path, run: runCommand}, nil
}

func (d *desktopNotifier) Notify(ctx context.Context, n Notification) error {
	body := n.Message
	if n.Detail != "" {
		body += "\n\n" + truncate(n.Detail, 400)
	}

	// Argumentos passados como slice: nunca interpolados em shell (AGENTS.md §3.4).
	args := []string{
		"--app-name=WatchFlow",
		"--urgency=" + urgencyFor(n.Kind),
		"--expire-time=" + expireForKind(n.Kind),
		n.Title,
		body,
	}

	if err := d.run(ctx, d.binary, args...); err != nil {
		return fmt.Errorf("falha ao executar %s: %w", notifySendBinary, err)
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

func urgencyFor(kind Kind) string {
	switch kind {
	case KindConflict:
		// Conflito exige ação humana: o alerta não pode expirar sozinho.
		return "critical"
	case KindError:
		return "normal"
	default:
		return "low"
	}
}

func expireForKind(kind Kind) string {
	if kind == KindConflict {
		// 0 = persiste até o usuário dispensar
		return "0"
	}
	return "10000"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
