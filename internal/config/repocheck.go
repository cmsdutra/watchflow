package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// As checagens deste arquivo são feitas por leitura direta do sistema de
// arquivos, sem invocar o binário do git e sem importar internal/providers/git
// — que importaria de volta este pacote, fechando um ciclo.

// usesGitActions informa se algum dos pipelines referenciados executa ações Git.
// Só nesse caso faz sentido exigir que a pasta seja um repositório.
func usesGitActions(w *WatcherConfig, pipelines map[string]Pipeline) bool {
	for _, name := range w.Pipelines {
		p, ok := pipelines[name]
		if !ok {
			continue
		}
		for _, step := range p.Steps {
			if strings.HasPrefix(step.Action, "git.") {
				return true
			}
		}
	}
	return false
}

// resolveGitDir devolve o diretório .git de um repositório, tratando o caso em
// que .git é um arquivo apontando para outro lugar (worktree ou submódulo).
func resolveGitDir(repoPath string) (string, error) {
	entry := filepath.Join(repoPath, ".git")

	info, err := os.Stat(entry)
	if err != nil {
		return "", fmt.Errorf("não é um repositório Git")
	}

	if info.IsDir() {
		return entry, nil
	}

	content, err := os.ReadFile(entry) // #nosec G304 -- caminho vem da configuração do usuário
	if err != nil {
		return "", fmt.Errorf("arquivo .git ilegível: %w", err)
	}

	text := strings.TrimSpace(string(content))
	if target, found := strings.CutPrefix(text, "gitdir:"); found {
		target = strings.TrimSpace(target)
		if !filepath.IsAbs(target) {
			target = filepath.Join(repoPath, target)
		}
		return filepath.Clean(target), nil
	}

	return "", fmt.Errorf("arquivo .git em formato inesperado")
}

// IsGitRepo informa se o caminho é a raiz de um repositório Git.
func IsGitRepo(repoPath string) bool {
	_, err := resolveGitDir(repoPath)
	return err == nil
}

// HasGitRemote informa se o repositório tem ao menos um remote configurado.
// Lê o arquivo de configuração do próprio repositório em vez de executar
// 'git remote', mantendo a validação barata e sem dependência de subprocesso.
func HasGitRemote(repoPath string) bool {
	gitDir, err := resolveGitDir(repoPath)
	if err != nil {
		return false
	}

	data, err := os.ReadFile(filepath.Join(gitDir, "config")) // #nosec G304 -- derivado da configuração
	if err != nil {
		return false
	}

	return strings.Contains(string(data), `[remote "`)
}

// RepoWarnings devolve avisos sobre os repositórios vigiados que não impedem o
// daemon de subir, mas que provavelmente não são o que o usuário pretendia.
//
// A ausência de remote é aviso, e não erro: um repositório só-local é uma
// configuração legítima — as ações git.safe_sync e git.push detectam a falta do
// remote e se pulam sozinhas, deixando apenas os commits automáticos.
func RepoWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}

	var warnings []string

	for i := range cfg.Watchers {
		w := &cfg.Watchers[i]
		if !w.IsEnabled() || !usesGitActions(w, cfg.Pipelines) {
			continue
		}

		path := w.ResolvedPath
		if path == "" {
			path = ExpandPath(w.Path)
		}

		if !IsGitRepo(path) {
			// Deliberadamente aviso, e não erro de validação: reprovar aqui
			// impediria o daemon inteiro de subir por causa de uma única pasta
			// mal configurada, derrubando junto os watchers que estão corretos.
			// Em execução, a falha fica contida — o job falha, o watcher vai a
			// DEGRADED e o usuário é notificado.
			warnings = append(warnings, fmt.Sprintf(
				"watcher '%s': '%s' NÃO é um repositório Git, mas o pipeline executa ações Git. "+
					"Nada será sincronizado até que isso seja corrigido. "+
					"Inicialize com: git -C '%s' init && git -C '%s' remote add origin <url>",
				w.Name, path, path, path))
			continue
		}

		if !HasGitRemote(path) {
			warnings = append(warnings, fmt.Sprintf(
				"watcher '%s': o repositório em '%s' não tem remote configurado; "+
					"os commits serão feitos localmente, mas nada será enviado. "+
					"Configure com: git -C '%s' remote add origin <url>",
				w.Name, path, path))
		}
	}

	return warnings
}
