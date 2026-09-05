package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/internal/config"
)

// gitRepo cria um repositório, opcionalmente com remote.
func gitRepo(t *testing.T, withRemote bool) string {
	t.Helper()

	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	content := "[core]\n\trepositoryformatversion = 0\n"
	if withRemote {
		content += "[remote \"origin\"]\n\turl = https://example.com/r.git\n"
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMinimalConfigNeedsOnlyNameAndPath cobre a redução da configuração
// mínima: os cinco passos do pipeline Git eram boilerplate que todo usuário
// copiava igual, e agora são implícitos.
func TestMinimalConfigNeedsOnlyNameAndPath(t *testing.T) {
	repo := gitRepo(t, true)

	yaml := "watchers:\n  - name: meu-cofre\n    path: " + repo + "\n"

	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatalf("a configuração mínima deveria ser válida: %v", err)
	}

	if len(cfg.Watchers) != 1 || cfg.Watchers[0].Name != "meu-cofre" {
		t.Fatalf("watcher não carregado: %+v", cfg.Watchers)
	}

	w := cfg.Watchers[0]
	if len(w.Pipelines) != 1 || w.Pipelines[0] != config.DefaultPipelineName {
		t.Errorf("esperava o pipeline padrão atribuído, obteve %v", w.Pipelines)
	}

	p, ok := cfg.Pipelines[config.DefaultPipelineName]
	if !ok {
		t.Fatal("pipeline padrão não foi injetado")
	}
	if len(p.Steps) != 5 {
		t.Errorf("esperava 5 passos no pipeline padrão, obteve %d", len(p.Steps))
	}
	// A ordem importa: a trava precisa ser checada antes de mexer no índice
	if p.Steps[0].Action != "git.check_locks" {
		t.Errorf("primeiro passo deveria ser git.check_locks, é %s", p.Steps[0].Action)
	}
	if p.Steps[len(p.Steps)-1].Action != "git.push" {
		t.Errorf("último passo deveria ser git.push, é %s", p.Steps[len(p.Steps)-1].Action)
	}

	// Defaults do daemon e do watcher continuam valendo
	if w.Debounce != "15s" || w.MaxWait != "60s" {
		t.Errorf("defaults do watcher não aplicados: %s / %s", w.Debounce, w.MaxWait)
	}
}

func TestUserPipelineIsNotOverwrittenByDefault(t *testing.T) {
	repo := gitRepo(t, true)

	yaml := `
watchers:
  - name: cofre
    path: ` + repo + `
pipelines:
  default:
    steps:
      - action: git.add
`
	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatal(err)
	}

	p := cfg.Pipelines[config.DefaultPipelineName]
	if len(p.Steps) != 1 {
		t.Errorf("o pipeline do usuário chamado 'default' foi sobrescrito: %+v", p.Steps)
	}
}

func TestExplicitPipelineSuppressesDefault(t *testing.T) {
	repo := gitRepo(t, true)

	yaml := `
watchers:
  - name: cofre
    path: ` + repo + `
    pipelines: [meu]
pipelines:
  meu:
    steps:
      - action: git.add
`
	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatal(err)
	}

	if _, injected := cfg.Pipelines[config.DefaultPipelineName]; injected {
		t.Error("o pipeline padrão não deveria ser injetado quando há um explícito")
	}
}

// TestNonGitRepoIsWarnedNotRejected documenta uma decisão de projeto: a pasta
// que não é repositório Git gera AVISO, não erro de validação.
//
// Reprovar impediria o daemon inteiro de subir por causa de uma única pasta mal
// configurada, derrubando junto os watchers corretos. Em execução a falha fica
// contida no job, o watcher vai a DEGRADED e o usuário é notificado.
func TestNonGitRepoIsWarnedNotRejected(t *testing.T) {
	plain := t.TempDir()

	yaml := "watchers:\n  - name: cofre\n    path: " + plain + "\n"

	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatalf("não deveria reprovar a configuração inteira: %v", err)
	}

	warnings := config.RepoWarnings(cfg)
	if len(warnings) != 1 {
		t.Fatalf("esperava 1 aviso, obteve %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "NÃO é um repositório Git") {
		t.Errorf("o aviso deveria identificar o problema: %s", warnings[0])
	}
	// Precisa dizer a consequência e como corrigir
	if !strings.Contains(warnings[0], "Nada será sincronizado") {
		t.Errorf("o aviso deveria explicar a consequência: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "git -C") {
		t.Errorf("o aviso deveria sugerir a correção: %s", warnings[0])
	}
}

// TestOneBrokenWatcherDoesNotBlockTheOthers é a razão de a checagem ser aviso.
func TestOneBrokenWatcherDoesNotBlockTheOthers(t *testing.T) {
	bom := gitRepo(t, true)
	ruim := t.TempDir()

	yaml := "watchers:\n" +
		"  - {name: bom, path: " + bom + "}\n" +
		"  - {name: ruim, path: " + ruim + "}\n"

	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatalf("um watcher quebrado não pode invalidar a configuração: %v", err)
	}
	if len(cfg.Watchers) != 2 {
		t.Fatalf("esperava os 2 watchers carregados, obteve %d", len(cfg.Watchers))
	}

	warnings := config.RepoWarnings(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ruim") {
		t.Errorf("esperava aviso apenas para o watcher quebrado: %v", warnings)
	}
}

// TestNonGitRepoAllowedWithoutGitActions garante que a exigência só vale quando
// o pipeline realmente executa ações Git.
func TestNonGitRepoAllowedWithoutGitActions(t *testing.T) {
	plain := t.TempDir()

	yaml := `
watchers:
  - name: cofre
    path: ` + plain + `
    pipelines: [nada]
pipelines:
  nada:
    steps:
      - action: alguma.acao.futura
`
	// A ação não existe no catálogo, mas em LoadBytes o registry pode estar
	// vazio; o que importa é não reprovar por causa do Git.
	_, err := config.LoadBytes([]byte(yaml), true)
	if err != nil && strings.Contains(err.Error(), "repositório Git") {
		t.Errorf("pipeline sem ações Git não deveria exigir repositório: %v", err)
	}
}

// TestMissingRemoteIsWarningNotError: um repositório só-local é configuração
// legítima — as ações de rede se pulam sozinhas.
func TestMissingRemoteIsWarningNotError(t *testing.T) {
	repo := gitRepo(t, false)

	yaml := "watchers:\n  - name: cofre\n    path: " + repo + "\n"

	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatalf("repositório sem remote não deveria reprovar: %v", err)
	}

	warnings := config.RepoWarnings(cfg)
	if len(warnings) != 1 {
		t.Fatalf("esperava 1 aviso sobre remote ausente, obteve %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "remote add origin") {
		t.Errorf("o aviso deveria dizer como corrigir: %s", warnings[0])
	}

	// Com remote, nenhum aviso
	if w := config.RepoWarnings(mustLoad(t, "watchers:\n  - name: c\n    path: "+gitRepo(t, true)+"\n")); len(w) != 0 {
		t.Errorf("repositório completo não deveria gerar aviso: %v", w)
	}
}

func TestGitRepoDetectionHandlesWorktreeFile(t *testing.T) {
	// .git como arquivo apontando para outro lugar (worktree ou submódulo)
	realGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(realGit, "config"),
		[]byte("[remote \"origin\"]\n\turl = https://example.com/r.git\n"), 0644); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+realGit+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if !config.IsGitRepo(repo) {
		t.Error("worktree com .git em arquivo deveria ser reconhecido")
	}
	if !config.HasGitRemote(repo) {
		t.Error("o remote do gitdir apontado deveria ser encontrado")
	}
}

func mustLoad(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(yaml), true)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
