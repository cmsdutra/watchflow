package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/locking"
	"github.com/watchflow/watchflow/internal/normalizer"
	"github.com/watchflow/watchflow/internal/pipeline"
	"github.com/watchflow/watchflow/internal/providers"
)

const (
	// preOpSuppressWindow cobre a duração da operação git que reescreve a árvore.
	preOpSuppressWindow = 15 * time.Second
	// echoTailWindow é o resíduo mantido após a operação para os eventos que o
	// kernel ainda estiver entregando.
	echoTailWindow = 2 * time.Second
)

func init() {
	_ = providers.Register(&SafeSyncAction{})
}

// SafeSyncAction implementa o protocolo 'Safe Git Sync' (WF-009) em substituição obrigatória a 'git pull --rebase'.
// Executa fetch, checagem de divergência com rev-list, fast-forward ou merge seguro sem edição,
// e aborta imediatamente (git merge --abort) caso haja qualquer conflito de linhas.
type SafeSyncAction struct{}

// Name retorna o identificador canônico da action.
func (a *SafeSyncAction) Name() string {
	return "git.safe_sync"
}

// Validate valida os parâmetros opcionais da action 'git.safe_sync'.
func (a *SafeSyncAction) Validate(params map[string]interface{}) error {
	if err := rejectUnknownParams("git.safe_sync", params, "remote", "branch"); err != nil {
		return err
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
	return nil
}

// Execute executa o protocolo Safe Git Sync com garantia anti-data-loss.
func (a *SafeSyncAction) Execute(ctx *providers.StepContext) (*providers.StepResult, error) {
	if ctx == nil || ctx.BasePath == "" {
		return nil, fmt.Errorf("caminho base do repositório não fornecido")
	}

	// Quando executada isoladamente (fora de um pipeline), a action garante ela
	// mesma a exclusão mútua sobre a árvore Git.
	if !ctx.RepoLockHeld {
		unlock := locking.DefaultRepoLocker.Lock(ctx.BasePath)
		defer unlock()
	}

	// 1. Identifica o remote alvo (padrão: "origin")
	remote := "origin"
	if ctx.StepParams != nil {
		if r, ok := ctx.StepParams["remote"].(string); ok && strings.TrimSpace(r) != "" {
			remote = strings.TrimSpace(r)
		}
	}

	// 2. Verifica se o remote existe no repositório local
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
			Output:  fmt.Sprintf("remote '%s' não configurado; safe_sync pulado", remote),
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
		errMsg := "repositório em detached HEAD ou branch desconhecido; safe_sync abortado por segurança"
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: errMsg,
		}, fmt.Errorf("%s", errMsg)
	}

	// 4. Executa git fetch <remote> <branch>
	fetchOut, fetchErr := runGit(ctx.Context, ctx.BasePath, "fetch", remote, branch)
	if fetchErr != nil {
		cat := pipeline.ClassifyError(fetchErr, nil)
		isTransient := cat == pipeline.CategoryTransient

		// Checa se o branch remoto simplesmente ainda não foi publicado no upstream
		if strings.Contains(strings.ToLower(fetchErr.Error()), "couldn't find remote ref") {
			return &providers.StepResult{
				Success: true,
				Output:  fmt.Sprintf("branch remoto '%s/%s' ainda não existe no upstream; pronto para push inicial", remote, branch),
			}, nil
		}

		return &providers.StepResult{
			Success:      false,
			TransientErr: isTransient,
			ErrorMessage: fmt.Sprintf("git fetch falhou: %v", fetchErr),
		}, fetchErr
	}

	// 5. Inspeciona o grafo de divergência entre origin/<branch> e HEAD
	remoteRef := fmt.Sprintf("%s/%s", remote, branch)
	revListOut, revListErr := runGit(ctx.Context, ctx.BasePath, "rev-list", "--left-right", "--count", remoteRef+"...HEAD")
	if revListErr != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao inspecionar grafo de divergência (%s...HEAD): %v", remoteRef, revListErr),
		}, revListErr
	}

	// Formato esperado de rev-list: "<behind>\t<ahead>"
	parts := strings.Fields(revListOut)
	if len(parts) < 2 {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("saída inesperada de rev-list: '%s'", revListOut),
		}, fmt.Errorf("saída inesperada de rev-list: '%s'", revListOut)
	}

	behind, errBehind := strconv.Atoi(parts[0])
	ahead, errAhead := strconv.Atoi(parts[1])
	if errBehind != nil || errAhead != nil {
		return &providers.StepResult{
			Success:      false,
			ErrorMessage: fmt.Sprintf("falha ao converter contagem de divergência: %v, %v", errBehind, errAhead),
		}, fmt.Errorf("contagem inválida: %s", revListOut)
	}

	// A partir daqui a árvore pode ser reescrita (merge, fast-forward ou abort):
	// a janela de supressão de eco precisa estar aberta ANTES da mutação.
	planned := a.planEchoSuppression(ctx.Context, ctx.BasePath, remoteRef)
	defer releaseEchoSuppression(planned)

	// Caso A: Em sincronia perfeita
	if behind == 0 && ahead == 0 {
		return &providers.StepResult{
			Success: true,
			Output:  fmt.Sprintf("repositório local em dia com '%s'", remoteRef),
		}, nil
	}

	// Caso B: Apenas commits locais à frente (remote não avançou)
	if behind == 0 && ahead > 0 {
		return &providers.StepResult{
			Success: true,
			Output:  fmt.Sprintf("local está %d commit(s) à frente de '%s' (pronto para push)", ahead, remoteRef),
		}, nil
	}

	// Caso C: Apenas o remote avançou (fast-forward limpo)
	if behind > 0 && ahead == 0 {
		ffOut, ffErr := runGit(ctx.Context, ctx.BasePath, "merge", "--ff-only", remoteRef)
		if ffErr != nil {
			return &providers.StepResult{
				Success:      false,
				ErrorMessage: fmt.Sprintf("fast-forward falhou inesperadamente: %v", ffErr),
			}, ffErr
		}
		return &providers.StepResult{
			Success: true,
			Output:  fmt.Sprintf("fast-forward concluído com sucesso (%d commits integrados): %s", behind, ffOut),
		}, nil
	}

	// Caso D: Divergência paralela (behind > 0 && ahead > 0) — Tenta merge seguro sem edição
	mergeOut, mergeErr := runGit(ctx.Context, ctx.BasePath, "merge", "--no-edit", remoteRef)
	if mergeErr == nil {
		// Merge automático de três vias concluído com sucesso e sem conflitos de linhas
		return &providers.StepResult{
			Success: true,
			Output:  fmt.Sprintf("merge de 3 vias concluído sem conflitos (%d remotos, %d locais): %s", behind, ahead, mergeOut),
		}, nil
	}

	// Conflito de merge detectado! Protocolo Anti-Data-Loss acionado imediatamente:
	abortOut, abortErr := runGit(context.Background(), ctx.BasePath, "merge", "--abort")
	if abortErr != nil {
		return &providers.StepResult{
			Success:      false,
			ConflictErr:  true,
			ErrorMessage: fmt.Sprintf("CRÍTICO: conflito de merge detectado e git merge --abort falhou (%v): %s", abortErr, abortOut),
		}, providers.ErrConflict
	}

	errMsg := fmt.Sprintf("conflito de merge detectado entre '%s' e local (%d remotos, %d locais); git merge --abort executado imediatamente com sucesso. Árvore de trabalho restaurada limpa.", remoteRef, behind, ahead)

	_ = fetchOut
	return &providers.StepResult{
		Success:      false,
		ConflictErr:  true,
		ErrorMessage: errMsg,
	}, providers.ErrConflict
}

// planEchoSuppression calcula, ANTES de qualquer mutação, quais arquivos a
// sincronização vai reescrever e abre a janela de supressão de eco para eles.
//
// Suprimir depois do merge chega tarde: o kernel já emitiu os eventos enquanto
// o git escrevia, e eles podem ter atravessado o filtro. Isso vale igualmente
// para 'git merge --abort', que também reescreve a árvore — e cujo eco
// realimentava o pipeline em repositórios com conflito.
func (a *SafeSyncAction) planEchoSuppression(ctx context.Context, basePath, remoteRef string) []string {
	diffOut, err := runGit(ctx, basePath, "diff", "--name-only", "HEAD", remoteRef)
	if err != nil || strings.TrimSpace(diffOut) == "" {
		return nil
	}

	var fullPaths []string
	for _, f := range strings.Split(strings.TrimSpace(diffOut), "\n") {
		if trimmed := strings.TrimSpace(f); trimmed != "" {
			fullPaths = append(fullPaths, filepath.Join(basePath, trimmed))
		}
	}

	if len(fullPaths) > 0 {
		normalizer.DefaultEchoSuppressor.SuppressMultiple(fullPaths, preOpSuppressWindow)
	}
	return fullPaths
}

// releaseEchoSuppression encurta a janela aberta por planEchoSuppression assim
// que a operação termina, deixando apenas o rabo curto necessário para os
// últimos eventos em trânsito. Sem isso, uma janela longa engoliria edições
// legítimas do usuário nos mesmos arquivos.
func releaseEchoSuppression(paths []string) {
	if len(paths) > 0 {
		// Por conteúdo: se o usuário editou algum destes arquivos enquanto a
		// sincronização rodava, o evento dele não pode ser confundido com eco.
		normalizer.DefaultEchoSuppressor.SuppressContentMultiple(paths, echoTailWindow)
	}
}
