package git

import (
	"fmt"
	"strings"

	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/providers"
)

func init() {
	_ = providers.Register(&PushAction{})
}

// PushAction implementa a action 'git.push' com detecção inteligente de falhas de rede transitórias e retry com backoff.
type PushAction struct{}

// Name retorna o identificador canônico da action.
func (a *PushAction) Name() string {
	return "git.push"
}

// Validate valida os parâmetros opcionais da action 'git.push'.
func (a *PushAction) Validate(params map[string]interface{}) error {
	if params == nil {
		return nil
	}
	if val, ok := params["remote"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'remote' deve ser uma string")
		}
	}
	if val, ok := params["branch"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'branch' deve ser uma string")
		}
	}
	if val, ok := params["set_upstream"]; ok {
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parâmetro 'set_upstream' deve ser um booleano")
		}
	}
	return nil
}

// Execute executa 'git push' tratando desconexão de rede como erro transitório.
func (a *PushAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
	defer unlock()

	// 1. Identifica o remote alvo (padrão: "origin")
	remote := "origin"
	if ctx.StepParams != nil {
		if r, ok := ctx.StepParams["remote"].(string); ok && strings.TrimSpace(r) != "" {
			remote = strings.TrimSpace(r)
		}
	}

	// 2. Verifica se o remote existe
	remotesList, err := runGit(ctx.Context, ctx.BasePath, "remote")
	if err != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao consultar remotes: %v", err),
		}, err
	}

	hasRemote := false
	for _, r := range strings.Split(remotesList, "\n") {
		if strings.TrimSpace(r) == remote {
			hasRemote = true
			break
		}
	}

	if !hasRemote {
		return &providers.StepResult{
			Success: true,
			Skipped: true,
			Output:  fmt.Sprintf("remote '%s' não configurado; git.push pulado", remote),
		}, nil
	}

	// 3. Determina o branch ativo
	branch := ""
	if ctx.StepParams != nil {
		if b, ok := ctx.StepParams["branch"].(string); ok && strings.TrimSpace(b) != "" {
			branch = strings.TrimSpace(b)
		}
	}
	if branch == "" {
		currentBranch, branchErr := runGit(ctx.Context, ctx.BasePath, "rev-parse", "--abbrev-ref", "HEAD")
		if branchErr != nil {
			return &providers.StepResult{
				Success:      false,
				ErrorMessage: fmt.Sprintf("falha ao identificar branch ativo: %v", branchErr),
			}, branchErr
		}
		branch = strings.TrimSpace(currentBranch)
	}

	if branch == "HEAD" || branch == "" {
		errMsg := "repositório em detached HEAD; git.push abortado por segurança"
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: errMsg,
		}, fmt.Errorf("%s", errMsg)
	}

	// 4. Prepara argumentos para git push
	args := []string{"push"}
	if ctx.StepParams != nil {
		if setUpstream, ok := ctx.StepParams["set_upstream"].(bool); ok && setUpstream {
			args = append(args, "-u")
		}
	}
	args = append(args, remote, branch)

	// 5. Executa git push
	pushOut, pushErr := runGit(ctx.Context, ctx.BasePath, args...)
	if pushErr != nil {
		errMsg := pushErr.Error()
		isTransient := isNetworkOrLockError(errMsg)

		return &providers.StepResult{
			Success:      false,
			TransientErr: isTransient,
			ErrorMessage: fmt.Sprintf("git push para '%s/%s' falhou: %s", remote, branch, errMsg),
		}, pushErr
	}

	if strings.Contains(pushOut, "Everything up-to-date") {
		return &providers.StepResult{
			Success: true,
			Output:  fmt.Sprintf("remote '%s/%s' já está atualizado (tudo em dia)", remote, branch),
		}, nil
	}

	return &providers.StepResult{
		Success: true,
		Output:  fmt.Sprintf("push para '%s/%s' concluído com sucesso: %s", remote, branch, pushOut),
	}, nil
}

func isNetworkOrLockError(errStr string) bool {
	lower := strings.ToLower(errStr)
	transientPatterns := []string{
		"could not resolve host",
		"unable to access",
		"connection refused",
		"network is unreachable",
		"i/o timeout",
		"operation timed out",
		"the remote end hung up unexpectedly",
		"failed to connect to",
		"ssl_error",
		"tls handshake",
		"resource temporarily unavailable",
		"temporary failure in name resolution",
		"index.lock",
		"non-fast-forward",
		"fetch first",
		"[rejected]",
	}

	for _, pattern := range transientPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
