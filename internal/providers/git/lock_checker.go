package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/providers"
)

// LockCheckerAction implementa a action 'git.check_locks', aguardando a liberação de travas externas (.git/index.lock).
type LockCheckerAction struct{}

// Name retorna o identificador canônico da action.
func (a *LockCheckerAction) Name() string {
	return "git.check_locks"
}

// Validate valida os parâmetros opcionais da action 'git.check_locks'.
func (a *LockCheckerAction) Validate(params map[string]interface{}) error {
	if params == nil {
		return nil
	}

	if val, ok := params["max_wait"]; ok {
		if _, err := parseDurationParam(val); err != nil {
			return fmt.Errorf("parâmetro 'max_wait' inválido: %w", err)
		}
	}

	if val, ok := params["poll_interval"]; ok {
		if _, err := parseDurationParam(val); err != nil {
			return fmt.Errorf("parâmetro 'poll_interval' inválido: %w", err)
		}
	}

	return nil
}

// Execute verifica e aguarda até que o arquivo .git/index.lock seja liberado.
func (a *LockCheckerAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: "caminho base do repositório não fornecido",
		}, fmt.Errorf("caminho base do repositório não fornecido")
	}

	gitDir, err := resolveGitDir(ctx.BasePath)
	if err != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("diretório Git inválido: %v", err),
		}, err
	}

	maxWait := 10 * time.Second
	pollInterval := 200 * time.Millisecond

	if ctx.StepParams != nil {
		if val, ok := ctx.StepParams["max_wait"]; ok {
			if d, err := parseDurationParam(val); err == nil && d > 0 {
				maxWait = d
			}
		}
		if val, ok := ctx.StepParams["poll_interval"]; ok {
			if d, err := parseDurationParam(val); err == nil && d > 0 {
				pollInterval = d
			}
		}
	}

	lockPath := filepath.Join(gitDir, "index.lock")

	// Verificação inicial rápida sem espera
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return &providers.StepResult{
			Success: true,
			Output:  "nenhuma trava .git/index.lock ativa",
		}, nil
	}

	// Trava detectada: aguarda com backoff/polling respeitando o timeout
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(maxWait)

	for {
		select {
		case <-ctx.Context.Done():
			return &providers.StepResult{
				Success:      false,
				TransientErr: true,
				ErrorMessage: fmt.Sprintf("espera por index.lock interrompida por cancelamento de contexto: %v", ctx.Context.Err()),
			}, ctx.Context.Err()

		case <-ticker.C:
			if _, err := os.Stat(lockPath); os.IsNotExist(err) {
				return &providers.StepResult{
					Success: true,
					Output:  "trava .git/index.lock liberada com sucesso",
				}, nil
			}

			if time.Now().After(deadline) {
				errMsg := fmt.Sprintf("trava .git/index.lock ainda ativa após aguardar %v", maxWait)
				return &providers.StepResult{
					Success:      false,
					TransientErr: true,
					ErrorMessage: errMsg,
				}, fmt.Errorf("%s", errMsg)
			}
		}
	}
}

func resolveGitDir(basePath string) (string, error) {
	cleanPath := filepath.Clean(basePath)
	realPath, err := filepath.EvalSymlinks(cleanPath)
	if err == nil {
		cleanPath = realPath
	}

	gitEntry := filepath.Join(cleanPath, ".git")
	fi, err := os.Stat(gitEntry)
	if err != nil {
		return "", fmt.Errorf("repositório Git não encontrado em '%s': %w", cleanPath, err)
	}

	if fi.IsDir() {
		return gitEntry, nil
	}

	// Se for arquivo (git worktree ou submodule), extrai a linha gitdir:
	content, err := os.ReadFile(gitEntry)
	if err != nil {
		return "", fmt.Errorf("falha ao ler arquivo .git em '%s': %w", cleanPath, err)
	}

	text := strings.TrimSpace(string(content))
	if strings.HasPrefix(text, "gitdir:") {
		target := strings.TrimSpace(strings.TrimPrefix(text, "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(cleanPath, target)
		}
		return filepath.Clean(target), nil
	}

	return gitEntry, nil
}

func parseDurationParam(val interface{}) (time.Duration, error) {
	switch v := val.(type) {
	case string:
		return time.ParseDuration(v)
	case time.Duration:
		return v, nil
	case int:
		return time.Duration(v) * time.Millisecond, nil
	case int64:
		return time.Duration(v) * time.Millisecond, nil
	case float64:
		return time.Duration(v) * time.Millisecond, nil
	default:
		return 0, fmt.Errorf("tipo de duração não suportado: %T", val)
	}
}
