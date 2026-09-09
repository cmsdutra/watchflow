package git

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/logger"
	"github.com/watchflow/watchflow/internal/proc"
	"github.com/watchflow/watchflow/internal/providers"
)

func init() {
	_ = providers.Register(&LockCheckerAction{})
	_ = providers.Register(&AddAction{})
	_ = providers.Register(&CommitAction{})
	_ = providers.Register(&PushAction{})
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
	if err := reg.Register(&SafeSyncAction{}); err != nil {
		return err
	}
	return reg.Register(&PushAction{})
}

// AddAction implementa a action 'git.add', adicionando alterações locais ao stage.
type AddAction struct{}

// Name retorna o identificador canônico da action.
func (a *AddAction) Name() string {
	return "git.add"
}

// Validate valida os parâmetros opcionais da action 'git.add'.
func (a *AddAction) Validate(params map[string]interface{}) error {
	if err := rejectUnknownParams("git.add", params, "pathspec", "all"); err != nil {
		return err
	}
	if val, ok := params["pathspec"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'pathspec' deve ser uma string")
		}
	}
	if val, ok := params["all"]; ok {
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parâmetro 'all' deve ser um booleano")
		}
	}
	return nil
}

// Execute executa 'git add' de forma idempotente. Se não houver alterações, pula o step.
func (a *AddAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	// Quando executada isoladamente (fora de um pipeline), a action garante ela
	// mesma a exclusão mútua sobre a árvore Git.
	if !ctx.RepoLockHeld {
		unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
		defer unlock()
	}

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

	// 3. Monta os argumentos de 'git add'
	args := []string{"add"}
	pathspec := ""
	if ctx.StepParams != nil {
		if customPath, ok := ctx.StepParams["pathspec"].(string); ok && customPath != "" {
			pathspec = customPath
		}
	}

	switch {
	case pathspec != "":
		args = append(args, pathspec)
	case ctx.StepParams != nil && ctx.StepParams["all"] == false:
		// all:false restringe o stage aos arquivos do lote que originou o job,
		// em vez de varrer a árvore inteira.
		if len(ctx.ChangedFiles) == 0 {
			return &providers.StepResult{
				Success: true,
				Skipped: true,
				Output:  "all:false sem arquivos no lote; nada a adicionar",
			}, nil
		}
		args = append(args, "--")
		args = append(args, ctx.ChangedFiles...)
	default:
		args = append(args, "-A")
	}

	addOut, err := runGit(ctx.Context, ctx.BasePath, args...)
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
	if err := rejectUnknownParams("git.commit", params, "message", "allow_empty"); err != nil {
		return err
	}
	if val, ok := params["message"]; ok {
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parâmetro 'message' deve ser uma string")
		}
	}
	if val, ok := params["allow_empty"]; ok {
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parâmetro 'allow_empty' deve ser um booleano")
		}
	}
	return nil
}

// Execute executa 'git commit' garantindo que não haja commits vazios.
func (a *CommitAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	// Quando executada isoladamente (fora de um pipeline), a action garante ela
	// mesma a exclusão mútua sobre a árvore Git.
	if !ctx.RepoLockHeld {
		unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
		defer unlock()
	}

	allowEmpty := false
	if ctx.StepParams != nil {
		if v, ok := ctx.StepParams["allow_empty"].(bool); ok {
			allowEmpty = v
		}
	}

	// 1. Verifica se há algo staged via git diff --cached --quiet
	_, diffErr := runGit(ctx.Context, ctx.BasePath, "diff", "--cached", "--quiet")
	if diffErr == nil && !allowEmpty {
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
	commitArgs := []string{"commit", "-m", commitMsg}
	if allowEmpty {
		commitArgs = append(commitArgs, "--allow-empty")
	}
	commitOut, err := runGit(ctx.Context, ctx.BasePath, commitArgs...)
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

// rejectUnknownParams reprova chaves não reconhecidas em 'params'. Sem isso, um
// parâmetro digitado errado era silenciosamente ignorado e o usuário acreditava
// ter configurado algo que nunca teve efeito.
func rejectUnknownParams(action string, params map[string]interface{}, allowed ...string) error {
	if params == nil {
		return nil
	}

	valid := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		valid[a] = true
	}

	var unknown []string
	for k := range params {
		if !valid[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	sort.Strings(unknown)
	return fmt.Errorf("parâmetro(s) desconhecido(s) em '%s': %s; aceitos: %s",
		action, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

func runGit(ctx context.Context, repoDir string, args ...string) (string, error) {
	cleanDir := filepath.Clean(repoDir)
	realDir, err := filepath.EvalSymlinks(cleanDir)
	if err == nil {
		cleanDir = realDir
	}

	// GIT_TERMINAL_PROMPT=0 (abaixo) cobre o prompt de terminal, mas não um
	// helper de credencial gráfico. No Windows o Git for Windows configura
	// 'credential.helper=manager' no gitconfig de sistema, e o GCM pode abrir
	// uma janela para renovar um token expirado — que ninguém vai responder,
	// porque o daemon roda sob o Agendador de Tarefas. Um push que falha volta
	// para a fila com backoff; um push que espera por uma janela invisível trava
	// o watcher para sempre.
	//
	// Vai como argumento, e não em GIT_CONFIG_COUNT/KEY/VALUE, para não colidir
	// com essas variáveis se já vierem do ambiente do processo.
	fullArgs := append([]string{"-c", "credential.interactive=false"}, args...)

	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Dir = cleanDir
	// O daemon roda sem console no Windows; sem isto, cada git abriria um
	// console novo e o usuário veria janelas piscando a cada sincronização.
	proc.HideConsole(cmd)
	cmd.Env = append(cmd.Environ(),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	out := sanitizeOutput(strings.TrimSpace(stdout.String()))
	errOut := sanitizeOutput(strings.TrimSpace(stderr.String()))

	log := logger.For("git").With(
		slog.String("repo", cleanDir),
		slog.String("args", strings.Join(args, " ")),
		slog.Duration("elapsed", elapsed),
	)

	if runErr != nil {
		log.Debug("comando git falhou", slog.String("stderr", errOut), slog.Any("error", runErr))
	} else {
		log.Debug("comando git executado", slog.String("stdout", out))
	}

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
// Delega ao catálogo central de padrões de segredo do pacote logger, de modo que
// a supressão exigida pela invariante de supressão de segredos (README) tenha uma única fonte de verdade.
func SanitizeGitOutput(s string) string {
	return logger.Redact(s)
}

func sanitizeOutput(s string) string {
	return SanitizeGitOutput(s)
}

// doubleBraceRe normaliza a sintaxe {{var}} para {var}. A configuração canônica
// distribuída usava chaves duplas, que a substituição simples deixava passar e
// produzia commits com o literal "{2026-01-01 10:00:00}" na mensagem.
var doubleBraceRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

func formatCommitMessage(template string, ctx *providers.StepContext) string {
	now := time.Now()
	msg := doubleBraceRe.ReplaceAllString(template, "{$1}")
	msg = strings.ReplaceAll(msg, "{timestamp}", now.Format("2006-01-02 15:04:05"))
	msg = strings.ReplaceAll(msg, "<timestamp>", now.Format("2006-01-02 15:04:05"))
	msg = strings.ReplaceAll(msg, "{iso_timestamp}", now.Format(time.RFC3339))
	msg = strings.ReplaceAll(msg, "{date}", now.Format("2006-01-02"))
	msg = strings.ReplaceAll(msg, "{time}", now.Format("15:04:05"))
	msg = strings.ReplaceAll(msg, "{watcher}", ctx.WatcherName)
	msg = strings.ReplaceAll(msg, "{files_count}", fmt.Sprintf("%d", len(ctx.ChangedFiles)))
	return msg
}
