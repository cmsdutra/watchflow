package testutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GitSandbox provê um ambiente isolado em diretório temporário para testes com Git.
type GitSandbox struct {
	T       testing.TB
	RootDir string
}

// NewGitSandbox cria um novo repositório Git isolado em t.TempDir().
func NewGitSandbox(t testing.TB) *GitSandbox {
	t.Helper()

	dir := t.TempDir()
	// Resolve links simbólicos para garantir caminhos canônicos no Linux
	realDir, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = realDir
	}

	sb := &GitSandbox{
		T:       t,
		RootDir: dir,
	}

	sb.MustRunGit("init", "-b", "main")
	sb.MustRunGit("config", "user.name", "WatchFlow Tester")
	sb.MustRunGit("config", "user.email", "tester@watchflow.local")
	sb.MustRunGit("config", "commit.gpgsign", "false")
	sb.MustRunGit("config", "core.autocrlf", "false")

	return sb
}

// NewBareRepo cria um repositório Git bare (central/remote) para testes de sincronização.
func NewBareRepo(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = realDir
	}
	cmd := exec.Command("git", "init", "--bare", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("falha ao inicializar bare repo: %v (%s)", err, string(out))
	}
	return dir
}

// CloneRepo clona um repositório fonte para um novo diretório temporário isolado.
func CloneRepo(t testing.TB, source string) *GitSandbox {
	t.Helper()
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = realDir
	}
	cmd := exec.Command("git", "clone", source, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("falha ao clonar repo '%s': %v (%s)", source, err, string(out))
	}
	sb := &GitSandbox{
		T:       t,
		RootDir: dir,
	}
	sb.MustRunGit("config", "user.name", "WatchFlow Tester")
	sb.MustRunGit("config", "user.email", "tester@watchflow.local")
	sb.MustRunGit("config", "commit.gpgsign", "false")
	sb.MustRunGit("config", "core.autocrlf", "false")
	return sb
}

// WriteFile cria ou sobrescreve um arquivo dentro do sandbox.
func (s *GitSandbox) WriteFile(relPath, content string) string {
	s.T.Helper()

	fullPath := filepath.Join(s.RootDir, relPath)
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		s.T.Fatalf("falha ao criar diretório para arquivo '%s': %v", relPath, err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		s.T.Fatalf("falha ao escrever arquivo '%s': %v", relPath, err)
	}

	return fullPath
}

// ReadFile lê o conteúdo de um arquivo do sandbox.
func (s *GitSandbox) ReadFile(relPath string) string {
	s.T.Helper()

	fullPath := filepath.Join(s.RootDir, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		s.T.Fatalf("falha ao ler arquivo '%s': %v", relPath, err)
	}

	return string(data)
}

// RemoveFile remove um arquivo dentro do sandbox.
func (s *GitSandbox) RemoveFile(relPath string) {
	s.T.Helper()

	fullPath := filepath.Join(s.RootDir, relPath)
	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		s.T.Fatalf("falha ao remover arquivo '%s': %v", relPath, err)
	}
}

// RunGit executa um comando Git dentro da raiz do sandbox e retorna stdout sanitizado.
func (s *GitSandbox) RunGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = s.RootDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())

	if err != nil {
		combined := errOut
		if combined == "" {
			combined = out
		}
		return out, fmt.Errorf("git %s falhou: %v (stderr: %s)", strings.Join(args, " "), err, combined)
	}

	return out, nil
}

// MustRunGit executa um comando Git e aborta o teste em caso de erro.
func (s *GitSandbox) MustRunGit(args ...string) string {
	s.T.Helper()
	out, err := s.RunGit(args...)
	if err != nil {
		s.T.Fatalf("MustRunGit falhou: %v", err)
	}
	return out
}

// CommitAll adiciona todas as alterações e cria um commit.
func (s *GitSandbox) CommitAll(msg string) string {
	s.T.Helper()
	s.MustRunGit("add", "-A")
	s.MustRunGit("commit", "-m", msg)
	return s.HeadHash()
}

// CreateIndexLock simula uma trava externa (.git/index.lock).
func (s *GitSandbox) CreateIndexLock() string {
	s.T.Helper()
	lockPath := filepath.Join(s.RootDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("12345"), 0644); err != nil {
		s.T.Fatalf("falha ao criar index.lock de teste: %v", err)
	}
	return lockPath
}

// RemoveIndexLock remove a trava externa (.git/index.lock).
func (s *GitSandbox) RemoveIndexLock() {
	s.T.Helper()
	lockPath := filepath.Join(s.RootDir, ".git", "index.lock")
	_ = os.Remove(lockPath)
}

// CommitCount retorna a contagem de commits no branch atual.
func (s *GitSandbox) CommitCount() int {
	s.T.Helper()
	out, err := s.RunGit("rev-list", "--count", "HEAD")
	if err != nil {
		return 0
	}
	count, err := strconv.Atoi(out)
	if err != nil {
		return 0
	}
	return count
}

// HeadHash retorna o hash SHA do commit no HEAD.
func (s *GitSandbox) HeadHash() string {
	s.T.Helper()
	return s.MustRunGit("rev-parse", "HEAD")
}
