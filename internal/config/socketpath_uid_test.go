package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestSocketPathIgnoraUIDSemUIDReal cobre o caso do Windows, onde os.Getuid()
// devolve -1 e '/run/user/${UID}' não é convenção nenhuma. Honrar o caminho
// literalmente apontaria o socket para '\run\user\-1' na raiz da unidade — e
// pior, um diretório com esse nome criado por acidente passaria a sequestrar o
// socket do daemon em toda execução seguinte.
func TestSocketPathIgnoraUIDSemUIDReal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cenário específico de plataforma sem UID real")
	}

	stateDir := t.TempDir()
	cfg := &Config{}
	cfg.Daemon.StateDir = stateDir
	cfg.Daemon.SocketPath = "/run/user/${UID}/watchflow.sock"

	applyDefaults(cfg)

	if strings.Contains(cfg.Daemon.SocketPath, "run") {
		t.Errorf("socket caiu na convenção /run/user do Linux: %q", cfg.Daemon.SocketPath)
	}
	if want := filepath.Join(stateDir, "watchflow.sock"); cfg.Daemon.SocketPath != want {
		t.Errorf("esperava fallback para o state_dir %q, obteve %q", want, cfg.Daemon.SocketPath)
	}
}

// TestSocketPathUsaRunUserComUIDReal garante que a guarda acima não desligou o
// comportamento correto no Linux.
func TestSocketPathUsaRunUserComUIDReal(t *testing.T) {
	uid := os.Getuid()
	if uid < 0 {
		t.Skip("plataforma sem UID real")
	}
	runUser := filepath.Join("/run/user", strconv.Itoa(uid))
	if _, err := os.Stat(runUser); err != nil {
		t.Skipf("'%s' não existe nesta máquina", runUser)
	}

	cfg := &Config{}
	cfg.Daemon.StateDir = t.TempDir()
	applyDefaults(cfg)

	if want := filepath.Join(runUser, "watchflow.sock"); cfg.Daemon.SocketPath != want {
		t.Errorf("esperava %q, obteve %q", want, cfg.Daemon.SocketPath)
	}
}
