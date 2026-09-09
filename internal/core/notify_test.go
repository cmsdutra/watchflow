package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/core"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/providers"
)

// conflictAction simula uma action que detecta conflito de merge e aborta,
// exatamente como git.safe_sync faz após executar 'git merge --abort'.
type conflictAction struct{ name string }

func (a *conflictAction) Name() string                          { return a.name }
func (a *conflictAction) Validate(map[string]interface{}) error { return nil }
func (a *conflictAction) Execute(*providers.StepContext) (*providers.StepResult, error) {
	return &providers.StepResult{
		Success:      false,
		ConflictErr:  true,
		ErrorMessage: "CONFLICT (content): Merge conflict in nota.md; merge --abort executado",
	}, providers.ErrConflict
}

// readNotifications extrai do log estruturado as entradas emitidas pelo
// subsistema de notificações.
func readNotifications(t *testing.T, dir string) []map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, logger.LogFileName))
	if err != nil {
		t.Fatalf("falha ao ler log: %v", err)
	}

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e["component"] == "notify" && e["kind"] != nil {
			out = append(out, e)
		}
	}
	return out
}

func runCoordinatorWithNotifications(t *testing.T, notifCfg config.NotificationConfig, action providers.ActionProvider) []map[string]any {
	t.Helper()

	if err := providers.DefaultRegistry.Register(action); err != nil {
		t.Fatalf("falha ao registrar action: %v", err)
	}
	t.Cleanup(func() { providers.DefaultRegistry.Unregister(action.Name()) })

	tempDir := t.TempDir()
	watchDir := filepath.Join(tempDir, "vault")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(tempDir, "state")

	// O logger precisa existir antes do coordenador: tanto o coordenador
	// quanto o notificador capturam o handler no momento da construção.
	closer, err := logger.Setup(logger.Options{Level: "debug", Dir: stateDir})
	if err != nil {
		t.Fatalf("falha ao configurar logger: %v", err)
	}
	t.Cleanup(func() {
		_ = closer.Close()
		logger.Discard()
	})

	cfg := &config.Config{
		Version: 1,
		Daemon: config.DaemonConfig{
			StateDir:               stateDir,
			SocketPath:             filepath.Join(tempDir, "wf.sock"),
			LogLevel:               "debug",
			MaxConcurrentPipelines: 1,
		},
		Notifications: notifCfg,
		Watchers: []config.WatcherConfig{{
			Name:             "vault",
			Path:             watchDir,
			ResolvedPath:     watchDir,
			Debounce:         "50ms",
			MaxWait:          "100ms",
			DebounceDuration: 50 * time.Millisecond,
			MaxWaitDuration:  100 * time.Millisecond,
			Pipelines:        []string{"pipe"},
		}},
		Pipelines: map[string]config.Pipeline{
			"pipe": {
				Timeout:         "10s",
				TimeoutDuration: 10 * time.Second,
				Steps:           []config.Step{{Action: action.Name()}},
			},
		},
	}

	coord, err := core.NewCoordinator(cfg, "test")
	if err != nil {
		t.Fatalf("falha ao criar coordenador: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = coord.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)

	if _, err := coord.Sync(ctx, "vault"); err != nil {
		t.Fatalf("falha ao solicitar sync: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := coord.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("falha no shutdown: %v", err)
	}

	return readNotifications(t, stateDir)
}

// TestConflictTriggersNotification cobre o invariante de conflito (README, "Invariantes de engenharia"): um
// conflito de merge DEVE disparar notificação ao usuário. Antes, o pacote
// internal/notify era um diretório vazio e o alerta simplesmente não existia.
func TestConflictTriggersNotification(t *testing.T) {
	notifs := runCoordinatorWithNotifications(t,
		config.NotificationConfig{Enabled: true, OnConflict: true, OnError: true, Backend: "log"},
		&conflictAction{name: "test.notify.conflict"})

	if len(notifs) == 0 {
		t.Fatal("nenhuma notificação emitida para um conflito de merge")
	}

	var found map[string]any
	for _, n := range notifs {
		if n["kind"] == "conflict" {
			found = n
			break
		}
	}
	if found == nil {
		t.Fatalf("esperava notificação de conflito, obteve: %v", notifs)
	}

	if found["watcher"] != "vault" {
		t.Errorf("watcher = %v", found["watcher"])
	}
	if found["level"] != "ERROR" {
		t.Errorf("conflito deveria ser nível ERROR, obteve %v", found["level"])
	}
	msg, _ := found["mensagem"].(string)
	if !strings.Contains(msg, "watchflow resume vault") {
		t.Errorf("notificação não orienta o usuário: %v", msg)
	}
}

// TestConflictNotificationRespectsOptOut garante que on_conflict=false silencia
// o alerta sem afetar o tratamento do conflito em si.
func TestConflictNotificationRespectsOptOut(t *testing.T) {
	notifs := runCoordinatorWithNotifications(t,
		config.NotificationConfig{Enabled: true, OnConflict: false, OnError: false, Backend: "log"},
		&conflictAction{name: "test.notify.conflict.optout"})

	for _, n := range notifs {
		if n["kind"] == "conflict" {
			t.Errorf("esperava nenhum alerta com on_conflict=false, obteve: %v", n)
		}
	}
}
