package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/watchflow/watchflow/internal/config"
)

// Checks específicos de Windows. Ficam num arquivo próprio (sem build tag: eles
// se autodesabilitam com INFO em outras plataformas) para que a suíte inteira
// continue compilando e sendo analisada em qualquer GOOS.

// scheduledTaskName é a tarefa por-usuário que scripts/install.ps1 registra no
// Agendador de Tarefas — o equivalente Windows da unidade systemd. É por-usuário
// justamente para não exigir privilégio de administrador.
const scheduledTaskName = "WatchFlow"

// checkScheduledTask é o equivalente Windows de checkServiceUnit.
//
// Sem ele o relatório fica cego no mesmo ponto que o check de socket deixa
// aberto no Linux: um daemon respondendo prova que *algum* processo está vivo,
// não que ele sobrevive ao fechamento do terminal ou ao próximo logon.
func checkScheduledTask() CheckResult {
	const category = "Serviço"

	if runtime.GOOS != "windows" {
		return CheckResult{
			Category: category,
			Name:     "Tarefa Agendada",
			Status:   StatusInfo,
			Message:  fmt.Sprintf("sistema operacional não-Windows (%s); Agendador de Tarefas não aplicável", runtime.GOOS),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "schtasks", "/Query", "/TN", scheduledTaskName, "/FO", "LIST").CombinedOutput()
	if err != nil {
		return CheckResult{
			Category: category,
			Name:     "Tarefa Agendada",
			Status:   StatusWarn,
			Message: fmt.Sprintf("tarefa '%s' não registrada no Agendador: o daemon não sobe sozinho no logon "+
				"e morre quando o terminal que o iniciou é fechado", scheduledTaskName),
			Remediation: "Registre a tarefa por-usuário com: powershell -ExecutionPolicy Bypass -File scripts\\install.ps1",
		}
	}

	// O 'Status' do schtasks é localizado, então a comparação usa os dois
	// rótulos mais comuns em vez de assumir a máquina em inglês.
	text := string(out)
	lower := strings.ToLower(text)
	running := strings.Contains(lower, "running") || strings.Contains(lower, "em execução")

	msg := fmt.Sprintf("tarefa '%s' registrada no Agendador (execução no logon, sem elevação)", scheduledTaskName)
	if running {
		msg += "; atualmente em execução"
	}

	return CheckResult{
		Category: category,
		Name:     "Tarefa Agendada",
		Status:   StatusOK,
		Message:  msg,
	}
}

// longPathsRegistryValue lê HKLM\SYSTEM\CurrentControlSet\Control\FileSystem\LongPathsEnabled.
// A leitura não exige privilégio; apenas a escrita exigiria, e por isso o doctor
// diagnostica sem corrigir.
func longPathsRegistryValue() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "reg", "query",
		`HKLM\SYSTEM\CurrentControlSet\Control\FileSystem`, "/v", "LongPathsEnabled").Output()
	if err != nil {
		return 0, fmt.Errorf("falha ao consultar o registro: %w", err)
	}

	// Formato: "    LongPathsEnabled    REG_DWORD    0x1"
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "LongPathsEnabled") {
			continue
		}
		fields := strings.Fields(line)
		raw := fields[len(fields)-1]
		v, convErr := strconv.ParseInt(strings.TrimPrefix(raw, "0x"), 16, 64)
		if convErr != nil {
			return 0, fmt.Errorf("valor inesperado '%s' para LongPathsEnabled", raw)
		}
		return int(v), nil
	}
	return 0, fmt.Errorf("valor LongPathsEnabled ausente na chave")
}

// checkLongPaths diagnostica o limite de MAX_PATH (260 caracteres), que trunca
// caminhos em cofres com hierarquia profunda.
//
// Habilitar a chave é escrita em HKLM e exige administrador, o que está fora do
// escopo do WatchFlow: aqui o check apenas detecta e orienta.
func checkLongPaths(cfg *config.Config) CheckResult {
	const category = "Caminhos"

	if runtime.GOOS != "windows" {
		return CheckResult{
			Category: category,
			Name:     "LongPathsEnabled",
			Status:   StatusInfo,
			Message:  fmt.Sprintf("sistema operacional não-Windows (%s); limite de MAX_PATH não aplicável", runtime.GOOS),
		}
	}

	value, err := longPathsRegistryValue()
	if err != nil {
		return CheckResult{
			Category: category,
			Name:     "LongPathsEnabled",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("não foi possível determinar LongPathsEnabled: %v", err),
			Remediation: "Verifique manualmente em HKLM\\SYSTEM\\CurrentControlSet\\Control\\FileSystem; " +
				"sem a chave, caminhos acima de 260 caracteres falham",
		}
	}

	if value == 1 {
		return CheckResult{
			Category: category,
			Name:     "LongPathsEnabled",
			Status:   StatusOK,
			Message:  "caminhos longos habilitados no sistema; o limite de 260 caracteres não se aplica",
		}
	}

	// Sem a chave o limite existe, mas só dói se algum cofre chegar perto dele.
	// Medir a folga real evita transformar um alerta genérico em ruído.
	deepest, deepestPath := deepestWatchedPath(cfg)
	msg := "caminhos longos desabilitados: caminhos acima de 260 caracteres falham"
	status := StatusInfo
	if deepest > 0 {
		msg = fmt.Sprintf("caminhos longos desabilitados; caminho mais profundo nos cofres tem %d caracteres (limite 260): '%s'",
			deepest, deepestPath)
		if deepest > 200 {
			status = StatusWarn
		}
	}

	return CheckResult{
		Category: category,
		Name:     "LongPathsEnabled",
		Status:   status,
		Message:  msg,
		Remediation: "Habilitar exige administrador e está fora do escopo do WatchFlow. Se precisar, peça a um " +
			"administrador para definir LongPathsEnabled=1 em " +
			"HKLM\\SYSTEM\\CurrentControlSet\\Control\\FileSystem, ou mantenha o cofre em um caminho mais raso",
	}
}

// deepestWatchedPath devolve o comprimento e o caminho mais longo encontrado nos
// diretórios vigiados, para dimensionar a folga até o limite de MAX_PATH.
func deepestWatchedPath(cfg *config.Config) (int, string) {
	if cfg == nil {
		return 0, ""
	}

	var maxLen int
	var maxPath string

	for _, w := range cfg.Watchers {
		target := w.ResolvedPath
		if target == "" {
			target = config.ExpandPath(w.Path)
		}

		// A varredura é apenas diagnóstica: erros de acesso são ignorados para
		// não transformar um check informativo em falha.
		_ = filepath.WalkDir(target, func(path string, _ os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // caminho inacessível não invalida o diagnóstico
			}
			if len(path) > maxLen {
				maxLen = len(path)
				maxPath = path
			}
			return nil
		})
	}

	return maxLen, maxPath
}

// checkLineEndings diagnostica a higiene de fim de linha dos cofres vigiados.
//
// O Git for Windows usa core.autocrlf=true por padrão. Isso não gera commits
// espontâneos — a conversão do Git é simétrica, e a árvore permanece limpa —,
// mas deixa o conteúdo do blob dependente da máquina que criou o arquivo. Num
// cofre compartilhado com Linux, o '.gitattributes' versionado é o que garante
// que o mesmo arquivo tenha o mesmo conteúdo nas duas pontas.
func checkLineEndings(cfg *config.Config) []CheckResult {
	const category = "Fim de Linha"

	if cfg == nil || len(cfg.Watchers) == 0 {
		return nil
	}

	var results []CheckResult

	for _, w := range cfg.Watchers {
		target := w.ResolvedPath
		if target == "" {
			target = config.ExpandPath(w.Path)
		}

		if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
			continue // não é repositório: outros checks já reportam isso
		}

		attrs := readGitAttributes(target)
		hasEOLRule := strings.Contains(attrs, "text=auto") || strings.Contains(attrs, "eol=")
		autocrlf := strings.ToLower(gitConfigValue(target, "core.autocrlf"))

		switch {
		case hasEOLRule:
			results = append(results, CheckResult{
				Category: category,
				Name:     w.Name,
				Status:   StatusOK,
				Message:  "'.gitattributes' fixa a convenção de fim de linha; o conteúdo é idêntico em Windows e Linux",
			})

		case runtime.GOOS == "windows" && autocrlf == "true":
			results = append(results, CheckResult{
				Category: category,
				Name:     w.Name,
				Status:   StatusWarn,
				Message: "sem regra de fim de linha no '.gitattributes' e com core.autocrlf=true: " +
					"arquivos criados aqui entram no histórico com CRLF e chegam assim na máquina Linux",
				Remediation: fmt.Sprintf("Versione um '.gitattributes' com '* text=auto eol=lf' em '%s' "+
					"e rode: git -C '%s' config --local core.autocrlf false", target, target),
			})

		default:
			results = append(results, CheckResult{
				Category: category,
				Name:     w.Name,
				Status:   StatusInfo,
				Message: fmt.Sprintf("sem regra de fim de linha no '.gitattributes' (core.autocrlf=%s); "+
					"suficiente enquanto o cofre não for compartilhado com outra plataforma", fallbackValue(autocrlf)),
				Remediation: "Se este cofre for sincronizado com Windows e Linux ao mesmo tempo, " +
					"versione um '.gitattributes' com '* text=auto eol=lf'",
			})
		}
	}

	return results
}

// checkCaseCollisions detecta arquivos versionados cujos nomes diferem apenas
// em caixa — legítimos no Linux, impossíveis de coexistir no Windows.
//
// O desfecho não é perda de dados: o 'git add' se recusa a estagiar o caminho
// colidido, então o histórico permanece íntegro e o pipeline nunca commita a
// sobrescrita. O que acontece é pior de diagnosticar do que de consertar: o
// clone traz só um dos arquivos para a árvore de trabalho, o outro fica
// invisível na máquina, e 'git status' passa a acusar uma modificação que não
// some nunca. Sem este check, isso se manifesta como um cofre que "está sempre
// sujo" e um arquivo que "sumiu", sem nada ligando as duas coisas.
func checkCaseCollisions(cfg *config.Config) []CheckResult {
	const category = "Caminhos"

	if runtime.GOOS != "windows" || cfg == nil {
		return nil
	}

	var results []CheckResult

	for _, w := range cfg.Watchers {
		target := w.ResolvedPath
		if target == "" {
			target = config.ExpandPath(w.Path)
		}
		if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, "git", "-C", target, "ls-files").Output()
		cancel()
		if err != nil {
			continue // outros checks já reportam repositório inacessível
		}

		vistos := make(map[string]string)
		var colisoes []string
		for _, path := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
			if path == "" {
				continue
			}
			chave := strings.ToLower(path)
			if anterior, existe := vistos[chave]; existe {
				colisoes = append(colisoes, fmt.Sprintf("'%s' e '%s'", anterior, path))
				continue
			}
			vistos[chave] = path
		}

		if len(colisoes) == 0 {
			continue
		}

		detalhe := strings.Join(colisoes, ", ")
		if len(colisoes) > 3 {
			detalhe = strings.Join(colisoes[:3], ", ") + fmt.Sprintf(" e mais %d", len(colisoes)-3)
		}

		results = append(results, CheckResult{
			Category: category,
			Name:     w.Name,
			Status:   StatusWarn,
			Message: fmt.Sprintf("%d par(es) de arquivos versionados diferem apenas em maiúsculas/minúsculas e não podem "+
				"coexistir neste sistema de arquivos: %s. Só um de cada par está na árvore de trabalho, e "+
				"'git status' acusará o outro como modificado permanentemente", len(colisoes), detalhe),
			Remediation: "O histórico não corre risco: o 'git add' se recusa a estagiar o caminho colidido. " +
				"Para usar os arquivos aqui, renomeie um de cada par em uma máquina com sistema de arquivos " +
				"sensível a maiúsculas (ex.: git mv) e sincronize",
		})
	}

	return results
}

// gitConfigValue lê uma chave do git config do repositório, devolvendo string
// vazia quando ela não está definida.
func gitConfigValue(repoPath, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fallbackValue(v string) string {
	if v == "" {
		return "não definido"
	}
	return v
}
