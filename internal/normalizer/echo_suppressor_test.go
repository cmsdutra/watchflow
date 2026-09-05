package normalizer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/providers"
	gitprovider "github.com/watchflow/watchflow/internal/providers/git"
	"github.com/watchflow/watchflow/internal/watcher"
	"github.com/watchflow/watchflow/tests/testutil"
)

func TestEchoSuppressor_BasicOperations(t *testing.T) {
	sup := normalizer.NewEchoSuppressor(100 * time.Millisecond)

	path := "/vault/notas/diario.md"
	if sup.IsSuppressed(path) {
		t.Errorf("arquivo não deveria estar suprimido inicialmente")
	}

	sup.Suppress(path, 100*time.Millisecond)
	if !sup.IsSuppressed(path) {
		t.Errorf("arquivo deveria estar suprimido após chamada a Suppress")
	}

	if sup.Count() != 1 {
		t.Errorf("esperava 1 item suprimido, encontrou %d", sup.Count())
	}

	// Aguarda expiração da janela temporal
	time.Sleep(150 * time.Millisecond)
	if sup.IsSuppressed(path) {
		t.Errorf("arquivo não deveria mais estar suprimido após expiração")
	}
	if sup.Count() != 0 {
		t.Errorf("esperava 0 itens após expiração, encontrou %d", sup.Count())
	}
}

func TestEchoSuppressor_SuppressMultipleAndClear(t *testing.T) {
	sup := normalizer.NewEchoSuppressor(500 * time.Millisecond)

	var files []string
	for i := 1; i <= 20; i++ {
		files = append(files, fmt.Sprintf("/vault/nota_%02d.md", i))
	}

	sup.SuppressMultiple(files, 500*time.Millisecond)
	if sup.Count() != 20 {
		t.Errorf("esperava 20 itens suprimidos, obteve %d", sup.Count())
	}

	for _, f := range files {
		if !sup.IsSuppressed(f) {
			t.Errorf("arquivo '%s' deveria estar suprimido", f)
		}
	}

	sup.Clear()
	if sup.Count() != 0 {
		t.Errorf("esperava 0 itens após Clear, obteve %d", sup.Count())
	}
}

func TestFilter_SuppressesEventsViaEchoSuppressor(t *testing.T) {
	tempDir := t.TempDir()
	sup := normalizer.NewEchoSuppressor(200 * time.Millisecond)

	f, err := normalizer.NewFilter(tempDir, nil)
	if err != nil {
		t.Fatalf("falha ao criar filtro: %v", err)
	}
	f.SetEchoSuppressor(sup)

	suppressedPath := filepath.Join(tempDir, "modificado_pelo_daemon.md")
	normalUserPath := filepath.Join(tempDir, "modificado_pelo_usuario.md")

	sup.Suppress(suppressedPath, 200*time.Millisecond)

	// Evento do arquivo suprimido deve ser descartado
	evSuppressed := watcher.FileEvent{
		WatcherName: "test-vault",
		Path:        suppressedPath,
		Op:          watcher.OpWrite,
		Timestamp:   time.Now(),
	}
	normSuppressed, okSuppressed := f.Normalize(evSuppressed)
	if okSuppressed || normSuppressed != nil {
		t.Errorf("esperava supressão do evento do daemon pelo filtro")
	}

	// Evento legítimo do usuário deve passar normalmente
	evUser := watcher.FileEvent{
		WatcherName: "test-vault",
		Path:        normalUserPath,
		Op:          watcher.OpWrite,
		Timestamp:   time.Now(),
	}
	normUser, okUser := f.Normalize(evUser)
	if !okUser || normUser == nil {
		t.Errorf("evento do usuário deveria ser aceito pelo filtro")
	}

	// Após expiração, o arquivo anteriormente suprimido volta a ser aceito
	time.Sleep(250 * time.Millisecond)
	normAfter, okAfter := f.Normalize(evSuppressed)
	if !okAfter || normAfter == nil {
		t.Errorf("evento deveria ser aceito após o término da janela de supressão")
	}
}

// TestSafeSync_EchoSuppressionOn20RemoteNotes valida rigorosamente o Critério de Aceite Principal de WF-011:
// "Execução de git pull/safe_sync com 20 notas alteradas gera 0 novos eventos no stream de debounce".
func TestSafeSync_EchoSuppressionOn20RemoteNotes(t *testing.T) {
	central := testutil.NewBareRepo(t)

	// 1. Cria repo inicial e sobe commit base
	initSb := testutil.NewGitSandbox(t)
	initSb.WriteFile("init.txt", "base")
	initSb.CommitAll("initial commit")
	initSb.MustRunGit("remote", "add", "origin", central)
	initSb.MustRunGit("push", "-u", "origin", "main")

	// 2. Cria Machine A e Machine B
	machA := testutil.CloneRepo(t, central)
	machB := testutil.CloneRepo(t, central)

	// 3. Machine B cria 20 arquivos Markdown e envia ao remote central
	for i := 1; i <= 20; i++ {
		machB.WriteFile(fmt.Sprintf("vault/nota_%02d.md", i), fmt.Sprintf("# Conteúdo Remoto %d", i))
	}
	machB.CommitAll("adiciona 20 notas remotas a partir da máquina B")
	machB.MustRunGit("push", "origin", "main")

	// 4. Limpa qualquer supressão residual e configura o filtro na Machine A
	normalizer.DefaultEchoSuppressor.Clear()

	filter, err := normalizer.NewFilter(machA.RootDir, nil)
	if err != nil {
		t.Fatalf("falha ao criar filtro: %v", err)
	}

	// 5. Machine A executa safe_sync (fast-forward das 20 notas)
	action := &gitprovider.SafeSyncAction{}
	ctx := &providers.StepContext{
		Context:     context.Background(),
		BasePath:    machA.RootDir,
		WatcherName: "machA-vault",
	}

	res, err := action.Execute(ctx)
	if err != nil {
		t.Fatalf("falha ao executar safe_sync na Máquina A: %v", err)
	}
	if !res.Success {
		t.Fatalf("esperava sucesso no safe_sync")
	}

	// 6. Simula a tempestade de eventos inotify gerados pelo kernel durante o merge das 20 notas
	var inotifyEvents []watcher.FileEvent
	for i := 1; i <= 20; i++ {
		filePath := filepath.Join(machA.RootDir, "vault", fmt.Sprintf("nota_%02d.md", i))
		inotifyEvents = append(inotifyEvents, watcher.FileEvent{
			WatcherName: "machA-vault",
			Path:        filePath,
			Op:          watcher.OpWrite,
			Timestamp:   time.Now(),
		})
	}

	// 7. Passa os eventos pelo Filter: TODOS os 20 devem ser descartados pelo EchoSuppressor!
	acceptedCount := 0
	for _, ev := range inotifyEvents {
		if _, ok := filter.Normalize(ev); ok {
			acceptedCount++
		}
	}

	if acceptedCount != 0 {
		t.Fatalf("CRITÉRIO DE ACEITE VIOLADO: %d eventos foram aceitos (esperava exatamente 0 eventos)", acceptedCount)
	}
}

// TestContentSuppressionLetsUserEditsThrough cobre a regressão introduzida ao
// abrir a janela de eco antes da operação git: uma supressão puramente por
// caminho cegava o filtro e engolia uma edição legítima do usuário feita no
// mesmo arquivo dentro da janela.
func TestContentSuppressionLetsUserEditsThrough(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nota.md")

	if err := os.WriteFile(file, []byte("conteúdo escrito pelo daemon"), 0644); err != nil {
		t.Fatal(err)
	}

	s := normalizer.NewEchoSuppressor(30 * time.Second)
	s.SuppressContent(file, 30*time.Second)

	if !s.IsSuppressed(file) {
		t.Error("o próprio conteúdo gravado pelo daemon deveria ser suprimido como eco")
	}

	// O usuário edita o mesmo arquivo dentro da janela
	if err := os.WriteFile(file, []byte("edição legítima do usuário"), 0644); err != nil {
		t.Fatal(err)
	}

	if s.IsSuppressed(file) {
		t.Error("edição do usuário foi confundida com eco e seria descartada silenciosamente")
	}
}

func TestContentSuppressionTreatsRemovalAsEcho(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "removido.md")

	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	s := normalizer.NewEchoSuppressor(30 * time.Second)
	s.SuppressContent(file, 30*time.Second)

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	if !s.IsSuppressed(file) {
		t.Error("remoção feita pelo daemon deveria continuar suprimida")
	}
}

func TestContentSuppressionExpires(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "nota.md")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	s := normalizer.NewEchoSuppressor(30 * time.Millisecond)
	s.SuppressContent(file, 30*time.Millisecond)

	time.Sleep(80 * time.Millisecond)
	if s.IsSuppressed(file) {
		t.Error("a janela de supressão deveria ter expirado")
	}
}
