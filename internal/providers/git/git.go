package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/providers"
)

var (
	// credRegex sanitiza credenciais embutidas em URLs Git para evitar vazamento em logs
	credRegex = regexp.MustCompile(`https?://[^/@\s]+@`)
)

func init() {
	_ = providers.Register(&LockCheckerAction{})
	_ = providers.Register(&AddAction{})
	_ = providers.Register(&CommitAction{})
}

// Register registra todas as ações do provedor Git no catálogo especificado.
func Register(reg *providers.Registry) error {
	if err := reg.Register(&LockCheckerAction{}); err != nil {
		return err
	}
	if err := reg.Register(&AddAction{}); err != nil {
		return err
	}
	if err := reg.Register(&CommitAction{}); err != nil {
		return err
	}
	return reg.Register(&SafeSyncAction{})
}

// AddAction implementa a action 'git.add', adicionando alterações locais ao stage.
type AddAction struct{}

// Name retorna o identificador canônico da action.
func (a *AddAction) Name() string {
	return "git.add"
}

// Validate valida os parâmetros opcionais da action 'git.add'.
func (a *AddAction) Validate(params map[string]interface{}) error {
	if params == nil {
		return nil
	}
	if val, ok := params["pathspec"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'pathspec' deve ser uma string")
		}
	}
	return nil
}

// Execute executa 'git add' de forma idempotente. Se não houver alterações, pula o step.
func (a *AddAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
	defer unlock()

	// 1. Inspeciona o status atual da árvore de trabalho
	statusOut, err := runGit(ctx.Context, ctx.BasePath, "status", "--porcelain")
	if err != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao verificar status: %v", err),
		}, err
	}

	if strings.TrimSpace(statusOut) == "" {
		return &providers.StepResult{
			Success: true,
			Skipped: true,
			Output:  "nenhuma alteração pendente no repositório",
		}, nil
	}

	// 2. Extrai lista de arquivos modificados
	lines := strings.Split(strings.TrimSpace(statusOut), "\n")
	var affected []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 3 {
			// Formato porcelain: XY filename
			filePath := strings.TrimSpace(trimmed[2:])
			affected = append(affected, filePath)
		}
	}

	// 3. Executa git add
	pathspec := "-A"
	if ctx.StepParams != nil {
		if customPath, ok := ctx.StepParams["pathspec"].(string); ok && customPath != "" {
			pathspec = customPath
		}
	}

	addOut, err := runGit(ctx.Context, ctx.BasePath, "add", pathspec)
	if err != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao adicionar arquivos ao stage: %v", err),
		}, err
	}

	output := fmt.Sprintf("%d arquivo(s) adicionados ao stage", len(affected))
	if addOut != "" {
		output += ": " + addOut
	}

	return &providers.StepResult{
		Success:       true,
		AffectedPaths: affected,
		Output:        output,
	}, nil
}

// CommitAction implementa a action 'git.commit', gravando alterações staged sem criar commits vazios.
type CommitAction struct{}

// Name retorna o identificador canônico da action.
func (a *CommitAction) Name() string {
	return "git.commit"
}

// Validate valida os parâmetros opcionais da action 'git.commit'.
func (a *CommitAction) Validate(params map[string]interface{}) error {
	if params == nil {
		return nil
	}
	if val, ok := params["message"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'message' deve ser uma string")
		}
	}
	return nil
}

// Execute executa 'git commit' garantindo que não haja commits vazios.
func (a *CommitAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
	defer unlock()

	// 1. Verifica se há algo staged via git diff --cached --quiet
	_, diffErr := runGit(ctx.Context, ctx.BasePath, "diff", "--cached", "--quiet")
	if diffErr == nil {
		// Código de saída 0 significa que não há alterações staged
		return &providers.StepResult{
			Success: true,
			Skipped: true,
			Output:  "nada a commitar (stage limpo)",
		}, nil
	}

	// 2. Formatação da mensagem de commit
	msgTemplate := "watchflow: auto-sync {timestamp}"
	if ctx.StepParams != nil {
		if customMsg, ok := ctx.StepParams["message"].(string); ok && customMsg != "" {
			msgTemplate = customMsg
		}
	}

	commitMsg := formatCommitMessage(msgTemplate, ctx)

	// 3. Executa o commit
	commitOut, err := runGit(ctx.Context, ctx.BasePath, "commit", "-m", commitMsg)
	if err != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao criar commit: %v", err),
		}, err
	}

	// 4. Captura o hash do commit criado
	shortHash, hashErr := runGit(ctx.Context, ctx.BasePath, "rev-parse", "--short", "HEAD")
	if hashErr == nil && shortHash != "" {
		commitOut = fmt.Sprintf("[%s] %s", shortHash, commitMsg)
	}

	return &providers.StepResult{
		Success: true,
		Output:  commitOut,
	}, nil
}

func runGit(ctx context.Context, repoDir string, args ...string) (string, error) {
	cleanDir := filepath.Clean(repoDir)
	realDir, err := filepath.EvalSymlinks(cleanDir)
	if err == nil {
		cleanDir = realDir
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cleanDir
	cmd.Env = append(cmd.Environ(),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	out := sanitizeOutput(strings.TrimSpace(stdout.String()))
	errOut := sanitizeOutput(strings.TrimSpace(stderr.String()))

	if runErr != nil {
		errMsg := errOut
		if errMsg == "" {
			errMsg = out
		}
		if errMsg == "" {
			errMsg = runErr.Error()
		}
		return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), errMsg)
	}

	return out, nil
}

// SanitizeGitOutput remove tokens e senhas de URLs Git em mensagens e logs.
func SanitizeGitOutput(s string) string {
	return credRegex.ReplaceAllString(s, "https://***@")
}

func sanitizeOutput(s string) string {
	return SanitizeGitOutput(s)
}

func formatCommitMessage(template string, ctx *providers.StepContext) string {
	now := time.Now()
	msg := template
	msg = strings.ReplaceAll(msg, "{timestamp}", now.Format("2006-01-02 15:04:05"))
	msg = strings.ReplaceAll(msg, "<timestamp>", now.Format("2006-01-02 15:04:05"))
	msg = strings.ReplaceAll(msg, "{iso_timestamp}", now.Format(time.RFC3339))
	msg = strings.ReplaceAll(msg, "{date}", now.Format("2006-01-02"))
	msg = strings.ReplaceAll(msg, "{time}", now.Format("15:04:05"))
	msg = strings.ReplaceAll(msg, "{watcher}", ctx.WatcherName)
	msg = strings.ReplaceAll(msg, "{files_count}", fmt.Sprintf("%d", len(ctx.ChangedFiles)))
	return msg
}
