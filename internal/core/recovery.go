package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/logger"
)

// QueueStore define os métodos necessários da fila para o procedimento de recuperação.
type QueueStore interface {
	ResetRunningJobs() (int64, error)
}

// RecoveryReport detalha o resultado da recuperação de estado executada no startup.
type RecoveryReport struct {
	JobsReset    int64    `json:"jobs_reset"`
	ReposCleaned []string `json:"repos_cleaned"`
	Warnings     []string `json:"warnings"`
}

// RunStartupRecovery executa a rotina de auto-cura no boot do daemon antes de iniciar os watchers.
// Recupera jobs órfãos que estavam em processamento (RUNNING) durante uma queda de energia ou SIGKILL,
// e limpa estados intermediários de merge pendente nos repositórios monitorados.
func RunStartupRecovery(ctx context.Context, store QueueStore, watchers []config.WatcherConfig) (*RecoveryReport, error) {
	log := logger.For("recovery")

	report := &RecoveryReport{
		ReposCleaned: make([]string, 0),
		Warnings:     make([]string, 0),
	}

	// 1. Reseta jobs órfãos presos em RUNNING na fila persistente
	if store != nil {
		resetCount, err := store.ResetRunningJobs()
		if err != nil {
			return nil, fmt.Errorf("falha ao resetar jobs em RUNNING durante startup: %w", err)
		}
		report.JobsReset = resetCount
		if resetCount > 0 {
			log.Warn("jobs órfãos recuperados de execução anterior interrompida",
				slog.Int64("quantidade", resetCount))
		}
	}

	// 2. Inspeciona a integridade das árvores Git de cada watcher configurado
	for _, w := range watchers {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}

		targetPath := w.ResolvedPath
		if targetPath == "" {
			targetPath = w.Path
		}
		if targetPath == "" {
			continue
		}

		cleanPath := filepath.Clean(targetPath)
		realPath, err := filepath.EvalSymlinks(cleanPath)
		if err == nil {
			cleanPath = realPath
		}

		fi, statErr := os.Stat(cleanPath)
		if statErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("watcher '%s': caminho '%s' inacessível: %v", w.Name, cleanPath, statErr))
			continue
		}
		if !fi.IsDir() {
			continue
		}

		// Adquire a trava de exclusão mútua do repositório
		unlock := locking.DefaultRepoLocker.Lock(cleanPath)

		cleaned, warn := checkAndCleanDanglingGitState(ctx, cleanPath, w.Name)
		unlock()

		if cleaned {
			log.Warn("estado de merge pendente encontrado e abortado no startup",
				slog.String("watcher", w.Name), slog.String("repo", cleanPath))
			report.ReposCleaned = append(report.ReposCleaned, cleanPath)
		}
		if warn != "" {
			report.Warnings = append(report.Warnings, warn)
		}
	}

	return report, nil
}

func checkAndCleanDanglingGitState(ctx context.Context, repoPath, watcherName string) (bool, string) {
	gitDir := filepath.Join(repoPath, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return false, ""
	}

	resolvedGitDir := gitDir
	if !fi.IsDir() {
		// Se for arquivo .git (submodule/worktree), extrai o gitdir
		content, readErr := os.ReadFile(gitDir)
		if readErr == nil {
			text := string(content)
			if len(text) > 8 && text[:7] == "gitdir:" {
				target := filepath.Clean(text[8:])
				if !filepath.IsAbs(target) {
					target = filepath.Join(repoPath, target)
				}
				resolvedGitDir = target
			}
		}
	}

	cleaned := false

	// Verifica se ficou preso em meio a um merge (MERGE_HEAD existe)
	mergeHeadPath := filepath.Join(resolvedGitDir, "MERGE_HEAD")
	if _, err := os.Stat(mergeHeadPath); err == nil {
		// Executa git merge --abort para restaurar a integridade
		cmd := exec.CommandContext(ctx, "git", "merge", "--abort")
		cmd.Dir = repoPath
		cmd.Env = append(cmd.Environ(), "LANG=C", "LC_ALL=C")
		if out, abortErr := cmd.CombinedOutput(); abortErr != nil {
			return false, fmt.Sprintf("watcher '%s': falha ao executar git merge --abort em repositório com MERGE_HEAD pendente: %v (%s)", watcherName, abortErr, string(out))
		}
		cleaned = true
	}

	// Aviso caso index.lock externo esteja presente
	indexLockPath := filepath.Join(resolvedGitDir, "index.lock")
	if _, err := os.Stat(indexLockPath); err == nil {
		return cleaned, fmt.Sprintf("watcher '%s': arquivo .git/index.lock detectado no startup (mantido intacto conforme regra anti-data-loss)", watcherName)
	}

	return cleaned, ""
}
