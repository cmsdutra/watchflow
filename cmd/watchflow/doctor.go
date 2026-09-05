package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/ipc"
	_ "modernc.org/sqlite"
)

// CheckStatus define os possíveis resultados de uma verificação do doctor.
type CheckStatus string

// Constantes que definem os estados possíveis de diagnóstico do doctor.
const (
	// StatusOK indica requisito atendido com sucesso.
	StatusOK CheckStatus = "OK"
	// StatusWarn indica aviso não impeditivo.
	StatusWarn CheckStatus = "WARN"
	// StatusFail indica falha crítica impeditiva.
	StatusFail CheckStatus = "FAIL"
	// StatusInfo indica mensagem meramente informativa.
	StatusInfo CheckStatus = "INFO"
)

// CheckResult consolida o diagnóstico de um requisito de sistema.
type CheckResult struct {
	Category    string
	Name        string
	Status      CheckStatus
	Message     string
	Remediation string
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnostica os pré-requisitos do sistema e a saúde do ambiente WatchFlow",
	Long:  `Inspeciona limites de inotify do kernel Linux, versão do Git, permissões de disco, integridade e tamanho do WAL da base SQLite, a unidade systemd do usuário e o estado do daemon.`,
	RunE:  runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	fmt.Println("\n🔍 Iniciando diagnóstico do ambiente WatchFlow...")
	fmt.Printf("Sistema Operacional: %s (%s)\n\n", runtime.GOOS, runtime.GOARCH)

	// Carrega configuração se disponível
	cfgPath := cfgFile
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}
	// Carregamento tolerante: o doctor é justamente a ferramenta que se usa
	// quando a configuração está errada. Usar config.Load aqui faria o
	// diagnóstico ficar cego para o problema que ele deveria apontar.
	cfg := loadConfigLeniently(cfgPath)

	var results []CheckResult

	// 1. Limites inotify do Kernel (Linux)
	results = append(results, checkInotifyLimits()...)

	// 2. Binário e Versão do Git
	results = append(results, checkGitRequirement())

	// 2b. Repositórios das pastas vigiadas
	results = append(results, checkWatchedRepos(cfg)...)

	// 3. Diretório de Estado e SQLite
	stateDir := "~/.local/state/watchflow"
	if cfg != nil && cfg.Daemon.StateDir != "" {
		stateDir = cfg.Daemon.StateDir
	}
	results = append(results, checkStateDirectory(stateDir)...)

	// 3b. Merge drivers referenciados pelos repositórios vigiados
	results = append(results, checkMergeDrivers(cfg)...)

	// 4. Unidade systemd do usuário
	results = append(results, checkServiceUnit())

	// 5. Socket IPC e Daemon
	sockPath := resolveSocketPath()
	results = append(results, checkIPCSocket(sockPath))

	// 6. Repositórios dos Watchers configurados
	if cfg != nil {
		results = append(results, checkWatchers(cfg)...)
	}

	// Renderiza relatório
	hasFail := renderDoctorReport(results)

	if hasFail {
		return fmt.Errorf("diagnóstico encontrou falhas críticas que impedem o funcionamento correto")
	}
	return nil
}

func checkInotifyLimits() []CheckResult {
	if runtime.GOOS != "linux" {
		return []CheckResult{
			{
				Category: "Kernel inotify",
				Name:     "max_user_watches",
				Status:   StatusInfo,
				Message:  fmt.Sprintf("sistema operacional não-Linux (%s); inotify não aplicável", runtime.GOOS),
			},
		}
	}

	var res []CheckResult

	// max_user_watches
	watchesPath := "/proc/sys/fs/inotify/max_user_watches"
	data, err := os.ReadFile(watchesPath)
	if err != nil {
		res = append(res, CheckResult{
			Category: "Kernel inotify",
			Name:     "max_user_watches",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("não foi possível ler '%s': %v", watchesPath, err),
		})
	} else {
		val, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		const recommendedWatches = 524288
		if val < recommendedWatches {
			res = append(res, CheckResult{
				Category:    "Kernel inotify",
				Name:        "max_user_watches",
				Status:      StatusWarn,
				Message:     fmt.Sprintf("limite atual (%d) é inferior ao recomendado (%d)", val, recommendedWatches),
				Remediation: fmt.Sprintf("Execute: sudo sysctl -w fs.inotify.max_user_watches=%d && echo fs.inotify.max_user_watches=%d | sudo tee -a /etc/sysctl.d/99-watchflow.conf", recommendedWatches, recommendedWatches),
			})
		} else {
			res = append(res, CheckResult{
				Category: "Kernel inotify",
				Name:     "max_user_watches",
				Status:   StatusOK,
				Message:  fmt.Sprintf("%d watches alocáveis (excelente)", val),
			})
		}
	}

	// max_user_instances
	instancesPath := "/proc/sys/fs/inotify/max_user_instances"
	instData, err := os.ReadFile(instancesPath)
	if err == nil {
		instVal, _ := strconv.Atoi(strings.TrimSpace(string(instData)))
		const minInstances = 128
		if instVal < minInstances {
			res = append(res, CheckResult{
				Category:    "Kernel inotify",
				Name:        "max_user_instances",
				Status:      StatusWarn,
				Message:     fmt.Sprintf("limite atual (%d) é inferior ao mínimo recomendado (%d)", instVal, minInstances),
				Remediation: fmt.Sprintf("Execute: sudo sysctl -w fs.inotify.max_user_instances=%d", minInstances),
			})
		} else {
			res = append(res, CheckResult{
				Category: "Kernel inotify",
				Name:     "max_user_instances",
				Status:   StatusOK,
				Message:  fmt.Sprintf("%d instâncias alocáveis", instVal),
			})
		}
	}

	return res
}

// loadConfigLeniently interpreta o YAML aplicando os defaults, mas sem as
// validações que dependem do sistema de arquivos. Devolve nil se o arquivo não
// existir ou tiver sintaxe inválida.
func loadConfigLeniently(path string) *config.Config {
	data, err := os.ReadFile(config.ExpandPath(path)) // #nosec G304 -- caminho informado pelo usuário
	if err != nil {
		return nil
	}

	cfg, err := config.LoadBytes(data, false)
	if err != nil {
		return nil
	}
	return cfg
}

// checkWatchedRepos verifica se cada pasta vigiada é de fato um repositório Git
// com remote. É a causa mais comum de o daemon subir e nunca sincronizar nada:
// a condição não aparece na configuração, só no primeiro pipeline.
func checkWatchedRepos(cfg *config.Config) []CheckResult {
	if cfg == nil || len(cfg.Watchers) == 0 {
		return nil
	}

	var results []CheckResult

	for i := range cfg.Watchers {
		w := &cfg.Watchers[i]
		if !w.IsEnabled() {
			continue
		}

		path := w.ResolvedPath
		if path == "" {
			path = config.ExpandPath(w.Path)
		}

		switch {
		case !config.IsGitRepo(path):
			results = append(results, CheckResult{
				Category:    "Repositórios Vigiados",
				Name:        w.Name,
				Status:      StatusFail,
				Message:     fmt.Sprintf("'%s' não é um repositório Git", path),
				Remediation: fmt.Sprintf("git -C '%s' init && git -C '%s' remote add origin <url>", path, path),
			})

		case !config.HasGitRemote(path):
			results = append(results, CheckResult{
				Category:    "Repositórios Vigiados",
				Name:        w.Name,
				Status:      StatusWarn,
				Message:     fmt.Sprintf("'%s' é um repositório Git, mas sem remote: os commits ficam só na máquina", path),
				Remediation: fmt.Sprintf("git -C '%s' remote add origin <url>", path),
			})

		default:
			results = append(results, CheckResult{
				Category: "Repositórios Vigiados",
				Name:     w.Name,
				Status:   StatusOK,
				Message:  fmt.Sprintf("repositório Git com remote em '%s'", path),
			})
		}
	}

	return results
}

func checkGitRequirement() CheckResult {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return CheckResult{
			Category:    "Dependências",
			Name:        "Git Binary",
			Status:      StatusFail,
			Message:     "o utilitário 'git' não foi encontrado no PATH do sistema",
			Remediation: "Instale o Git via gerenciador de pacotes (ex.: sudo apt install git)",
		}
	}

	cmd := exec.Command("git", "--version")
	out, err := cmd.Output()
	if err != nil {
		return CheckResult{
			Category:    "Dependências",
			Name:        "Git Binary",
			Status:      StatusFail,
			Message:     fmt.Sprintf("falha ao executar git (%s): %v", gitPath, err),
			Remediation: "Verifique a integridade do pacote Git instalado no sistema",
		}
	}

	verStr := strings.TrimSpace(string(out))
	re := regexp.MustCompile(`version\s+(\d+)\.(\d+)`)
	matches := re.FindStringSubmatch(verStr)
	if len(matches) >= 3 {
		major, _ := strconv.Atoi(matches[1])
		minor, _ := strconv.Atoi(matches[2])
		if major < 2 || (major == 2 && minor < 30) {
			return CheckResult{
				Category:    "Dependências",
				Name:        "Git Version",
				Status:      StatusWarn,
				Message:     fmt.Sprintf("versão instalada (%s) é antiga; recomendado Git >= 2.30", verStr),
				Remediation: "Atualize o Git para uma versão mais recente",
			}
		}
	}

	return CheckResult{
		Category: "Dependências",
		Name:     "Git Version",
		Status:   StatusOK,
		Message:  fmt.Sprintf("%s (%s)", verStr, gitPath),
	}
}

func checkStateDirectory(stateDir string) []CheckResult {
	expanded := config.ExpandPath(stateDir)
	var res []CheckResult

	// Permissões de escrita no stateDir
	if err := os.MkdirAll(expanded, 0700); err != nil {
		res = append(res, CheckResult{
			Category:    "Armazenamento",
			Name:        "State Directory",
			Status:      StatusFail,
			Message:     fmt.Sprintf("falha ao criar ou acessar diretório de estado '%s': %v", expanded, err),
			Remediation: fmt.Sprintf("Verifique as permissões de acesso ao caminho '%s'", expanded),
		})
		return res
	}

	testFile := filepath.Join(expanded, ".write_test")
	if err := os.WriteFile(testFile, []byte("ok"), 0600); err != nil {
		res = append(res, CheckResult{
			Category:    "Armazenamento",
			Name:        "State Directory",
			Status:      StatusFail,
			Message:     fmt.Sprintf("permissão de escrita negada em '%s': %v", expanded, err),
			Remediation: fmt.Sprintf("Corrija o proprietário do diretório: chown -R $USER:$USER %s", expanded),
		})
		return res
	}
	_ = os.Remove(testFile)

	res = append(res, CheckResult{
		Category: "Armazenamento",
		Name:     "State Directory",
		Status:   StatusOK,
		Message:  fmt.Sprintf("diretório acessível com permissão de escrita (%s)", expanded),
	})

	// Integridade do SQLite
	dbPath := filepath.Join(expanded, "state.db")
	if _, err := os.Stat(dbPath); err == nil {
		db, dbErr := sql.Open("sqlite", dbPath)
		if dbErr != nil {
			res = append(res, CheckResult{
				Category: "Armazenamento",
				Name:     "SQLite Integrity",
				Status:   StatusFail,
				Message:  fmt.Sprintf("falha ao abrir banco '%s': %v", dbPath, dbErr),
			})
		} else {
			defer func() { _ = db.Close() }()
			var checkResult string
			row := db.QueryRow("PRAGMA integrity_check;")
			if scanErr := row.Scan(&checkResult); scanErr != nil || checkResult != "ok" {
				res = append(res, CheckResult{
					Category:    "Armazenamento",
					Name:        "SQLite Integrity",
					Status:      StatusFail,
					Message:     fmt.Sprintf("corrupção detectada no SQLite (%s): %s", dbPath, checkResult),
					Remediation: "Restaure um backup de state.db ou remova-o para recriação automática pelo daemon",
				})
			} else {
				res = append(res, CheckResult{
					Category: "Armazenamento",
					Name:     "SQLite Integrity",
					Status:   StatusOK,
					Message:  "banco íntegro (PRAGMA integrity_check: ok, modo WAL)",
				})
			}
		}

		res = append(res, checkWALSize(dbPath))
	} else {
		res = append(res, CheckResult{
			Category: "Armazenamento",
			Name:     "SQLite Integrity",
			Status:   StatusOK,
			Message:  "banco state.db ainda não inicializado (será criado no primeiro start)",
		})
	}

	return res
}

// builtinMergeDrivers são os drivers que o Git resolve sozinho; só os demais
// exigem uma definição em 'merge.<nome>.driver'.
var builtinMergeDrivers = map[string]bool{"text": true, "binary": true, "union": true}

// checkMergeDrivers detecta a metade faltante de uma configuração de merge
// driver personalizada.
//
// O .gitattributes é versionado e chega junto com o clone; a definição do driver
// mora em 'git config --local' e NÃO viaja. Quando só a primeira metade existe,
// o Git não reclama: ele volta silenciosamente ao merge de texto padrão e produz
// conflito no primeiro merge concorrente — que, para o WatchFlow, significa
// watcher em CONFLICT_HALTED. A armadilha se rearma a cada clone novo.
func checkMergeDrivers(cfg *config.Config) []CheckResult {
	if cfg == nil || len(cfg.Watchers) == 0 {
		return nil
	}

	var results []CheckResult

	for i := range cfg.Watchers {
		w := &cfg.Watchers[i]
		if !w.IsEnabled() {
			continue
		}

		path := w.ResolvedPath
		if path == "" {
			path = config.ExpandPath(w.Path)
		}
		if !config.IsGitRepo(path) {
			// checkWatchedRepos já reporta esse caso; repetir aqui só polui.
			continue
		}

		drivers := mergeDriversInAttributes(readGitAttributes(path))
		if len(drivers) == 0 {
			continue
		}

		var missing []string
		for _, name := range drivers {
			if !hasMergeDriver(path, name) {
				missing = append(missing, name)
			}
		}

		if len(missing) > 0 {
			results = append(results, CheckResult{
				Category: "Merge Drivers",
				Name:     w.Name,
				Status:   StatusWarn,
				Message: fmt.Sprintf("'.gitattributes' referencia o driver '%s', que não está definido neste clone; o Git cai no merge padrão e gera conflito sem avisar",
					strings.Join(missing, "', '")),
				Remediation: fmt.Sprintf("Defina o driver no repositório, por exemplo: git -C '%s' config --local merge.%s.driver '<comando>'", path, missing[0]),
			})
			continue
		}

		results = append(results, CheckResult{
			Category: "Merge Drivers",
			Name:     w.Name,
			Status:   StatusOK,
			Message:  fmt.Sprintf("driver(s) '%s' definido(s) neste clone", strings.Join(drivers, "', '")),
		})
	}

	return results
}

// readGitAttributes concatena as duas origens de atributos que valem para o
// repositório inteiro: a versionada e a local. Atributos em subdiretórios ficam
// de fora — no uso típico (um cofre) as regras vivem na raiz.
func readGitAttributes(repoPath string) string {
	var b strings.Builder
	for _, rel := range []string{".gitattributes", filepath.Join(".git", "info", "attributes")} {
		data, err := os.ReadFile(filepath.Join(repoPath, rel)) // #nosec G304 -- caminho derivado da configuração
		if err == nil {
			b.Write(data)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// mergeDriversInAttributes extrai os nomes de driver personalizados declarados
// como 'merge=<nome>'. As formas '-merge' e '!merge' não nomeiam driver algum.
func mergeDriversInAttributes(content string) []string {
	seen := make(map[string]bool)
	var names []string

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		// O primeiro campo é o padrão de caminho, nunca um atributo.
		for _, field := range fields[1:] {
			name, ok := strings.CutPrefix(field, "merge=")
			if !ok || name == "" || builtinMergeDrivers[name] || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}

	sort.Strings(names)
	return names
}

// hasMergeDriver informa se o clone define o comando do driver. Só '.driver'
// importa: '.name' é rótulo descritivo e sua ausência não quebra o merge.
func hasMergeDriver(repoPath, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "config", "--get", "merge."+name+".driver")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// serviceUnitName é a unidade de usuário instalada por scripts/install.sh.
const serviceUnitName = "watchflow.service"

// checkServiceUnit inspeciona a unidade systemd do usuário.
//
// Existe porque o check de socket logo abaixo só prova que *algum* daemon
// responde — ele não distingue um processo manual preso a um terminal de um
// serviço gerenciado, nem enxerga uma unidade em loop de restart, que falha e
// volta a cada RestartSec sem nunca chegar a executar o binário.
func checkServiceUnit() CheckResult {
	const category = "Serviço"

	if runtime.GOOS != "linux" {
		return CheckResult{
			Category: category,
			Name:     "systemd Unit",
			Status:   StatusInfo,
			Message:  fmt.Sprintf("sistema operacional não-Linux (%s); unidade systemd não aplicável", runtime.GOOS),
		}
	}

	if _, err := exec.LookPath("systemctl"); err != nil {
		return CheckResult{
			Category: category,
			Name:     "systemd Unit",
			Status:   StatusInfo,
			Message:  "systemctl não encontrado no PATH; o daemon precisa ser iniciado manualmente",
		}
	}

	props, err := systemdProperties(serviceUnitName)
	if err != nil {
		return CheckResult{
			Category: category,
			Name:     "systemd Unit",
			Status:   StatusInfo,
			Message:  fmt.Sprintf("não foi possível consultar o systemd do usuário: %v", err),
		}
	}

	return evaluateServiceUnit(props)
}

// evaluateServiceUnit traduz as propriedades da unidade em diagnóstico. Fica
// separada da coleta para que cada estado — inclusive o loop de reinício, que é
// incômodo de reproduzir de propósito — seja coberto por teste.
func evaluateServiceUnit(props map[string]string) CheckResult {
	const category = "Serviço"

	// LoadState distingue "unidade inexistente" de "unidade com problema": sem
	// essa checagem, quem roda o daemon à mão receberia um alarme falso.
	if props["LoadState"] == "not-found" {
		return CheckResult{
			Category:    category,
			Name:        "systemd Unit",
			Status:      StatusInfo,
			Message:     fmt.Sprintf("unidade '%s' não instalada; o daemon depende de inicialização manual e morre junto com o terminal (SIGHUP)", serviceUnitName),
			Remediation: "Instale o serviço com ./install.sh para que o daemon sobreviva ao fechamento do terminal",
		}
	}

	restarts := props["NRestarts"]
	autoStart := "sem auto-start no login"
	if props["UnitFileState"] == "enabled" {
		autoStart = "auto-start habilitado"
	}

	switch {
	// activating + auto-restart é o loop de falha: a unidade nunca alcança
	// 'active', então nem status nem socket denunciam o problema.
	case props["ActiveState"] == "activating" && props["SubState"] == "auto-restart":
		return CheckResult{
			Category:    category,
			Name:        "systemd Unit",
			Status:      StatusFail,
			Message:     fmt.Sprintf("unidade em loop de reinício (%s restarts, Result=%s); o daemon nunca chega a subir pelo serviço", restarts, props["Result"]),
			Remediation: fmt.Sprintf("Inspecione a causa com: journalctl --user -u %s -n 50 --no-pager", serviceUnitName),
		}

	case props["ActiveState"] == "failed":
		return CheckResult{
			Category:    category,
			Name:        "systemd Unit",
			Status:      StatusFail,
			Message:     fmt.Sprintf("unidade em estado 'failed' (Result=%s)", props["Result"]),
			Remediation: fmt.Sprintf("Inspecione a causa com: journalctl --user -u %s -n 50 --no-pager", serviceUnitName),
		}

	case props["ActiveState"] == "active":
		msg := fmt.Sprintf("unidade ativa e gerenciada pelo systemd (%s)", autoStart)
		if restarts != "" && restarts != "0" {
			// Reinícios acumulados com a unidade ativa indicam instabilidade
			// intermitente, que o estado atual sozinho esconde.
			return CheckResult{
				Category:    category,
				Name:        "systemd Unit",
				Status:      StatusWarn,
				Message:     fmt.Sprintf("%s, mas com %s reinícios acumulados", msg, restarts),
				Remediation: fmt.Sprintf("Verifique quedas anteriores com: journalctl --user -u %s -n 50 --no-pager", serviceUnitName),
			}
		}
		return CheckResult{Category: category, Name: "systemd Unit", Status: StatusOK, Message: msg}

	default:
		return CheckResult{
			Category:    category,
			Name:        "systemd Unit",
			Status:      StatusInfo,
			Message:     fmt.Sprintf("unidade instalada mas parada (ActiveState=%s, %s)", props["ActiveState"], autoStart),
			Remediation: fmt.Sprintf("Inicie o serviço com: systemctl --user start %s", serviceUnitName),
		}
	}
}

// systemdProperties lê propriedades da unidade via 'systemctl show', que sai com
// status 0 mesmo para unidade inexistente — daí a distinção ficar por conta de
// LoadState, e não do código de saída.
func systemdProperties(unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", "--user", "show", unit,
		"--property=LoadState",
		"--property=ActiveState",
		"--property=SubState",
		"--property=UnitFileState",
		"--property=NRestarts",
		"--property=Result",
	)

	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found {
			props[key] = value
		}
	}
	return props, nil
}

// walHighWaterMark é a marca d'água esperada do WAL em operação normal:
// wal_autocheckpoint tem 1000 páginas por padrão e o page_size é de 4 KiB, então
// o arquivo estaciona perto de 4 MiB e só encolhe num checkpoint TRUNCATE. Um
// WAL muito acima disso indica checkpoint represado por um leitor de vida longa,
// e não simples acúmulo.
const walHighWaterMark = 16 << 20

// checkWALSize compara o WAL com o banco. Não é um teste de corrupção — o
// integrity_check já cobre isso — e sim de crescimento: um WAL represado atrasa
// a recuperação no próximo start e cresce sem teto prático.
func checkWALSize(dbPath string) CheckResult {
	walPath := dbPath + "-wal"

	walInfo, err := os.Stat(walPath)
	if err != nil {
		// Ausência do WAL é o estado esperado com o daemon parado após um
		// encerramento gracioso: o checkpoint TRUNCATE zera e remove o arquivo.
		return CheckResult{
			Category: "Armazenamento",
			Name:     "SQLite WAL",
			Status:   StatusOK,
			Message:  "sem WAL pendente (checkpoint aplicado no último encerramento)",
		}
	}

	walSize := walInfo.Size()
	if walSize > walHighWaterMark {
		return CheckResult{
			Category:    "Armazenamento",
			Name:        "SQLite WAL",
			Status:      StatusWarn,
			Message:     fmt.Sprintf("WAL com %s, acima da marca d'água esperada de %s: o checkpoint pode estar represado por um leitor de vida longa", humanBytes(walSize), humanBytes(walHighWaterMark)),
			Remediation: "Pare o daemon ('watchflow stop') para forçar o checkpoint TRUNCATE; se o WAL persistir grande, reporte o caso",
		}
	}

	return CheckResult{
		Category: "Armazenamento",
		Name:     "SQLite WAL",
		Status:   StatusOK,
		Message:  fmt.Sprintf("WAL com %s (dentro da marca d'água de %s)", humanBytes(walSize), humanBytes(walHighWaterMark)),
	}
}

// humanBytes formata tamanhos em unidades binárias legíveis no relatório.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func checkIPCSocket(sockPath string) CheckResult {
	cleanPath := config.ExpandPath(sockPath)
	client := ipc.NewClient(cleanPath)

	if client.IsDaemonRunning() {
		return CheckResult{
			Category: "Daemon & IPC",
			Name:     "Socket Connection",
			Status:   StatusOK,
			Message:  fmt.Sprintf("daemon ativo e respondendo no socket '%s'", cleanPath),
		}
	}

	sockDir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(sockDir, 0700); err != nil {
		return CheckResult{
			Category:    "Daemon & IPC",
			Name:        "Socket Directory",
			Status:      StatusFail,
			Message:     fmt.Sprintf("diretório do socket '%s' inacessível: %v", sockDir, err),
			Remediation: fmt.Sprintf("Crie o diretório com permissões do usuário: mkdir -p %s && chmod 700 %s", sockDir, sockDir),
		}
	}

	return CheckResult{
		Category: "Daemon & IPC",
		Name:     "Daemon Status",
		Status:   StatusInfo,
		Message:  fmt.Sprintf("daemon não está em execução (socket '%s' livre para inicialização)", cleanPath),
	}
}

func checkWatchers(cfg *config.Config) []CheckResult {
	var res []CheckResult

	for _, w := range cfg.Watchers {
		targetPath := w.ResolvedPath
		if targetPath == "" {
			targetPath = config.ExpandPath(w.Path)
		}

		fi, err := os.Stat(targetPath)
		if err != nil {
			res = append(res, CheckResult{
				Category:    "Watchers",
				Name:        w.Name,
				Status:      StatusFail,
				Message:     fmt.Sprintf("diretório vigiado '%s' não existe: %v", targetPath, err),
				Remediation: fmt.Sprintf("Crie o diretório com: mkdir -p '%s'", targetPath),
			})
			continue
		}
		if !fi.IsDir() {
			res = append(res, CheckResult{
				Category: "Watchers",
				Name:     w.Name,
				Status:   StatusFail,
				Message:  fmt.Sprintf("caminho '%s' existe mas não é um diretório", targetPath),
			})
			continue
		}

		// Checa repositório Git
		gitDir := filepath.Join(targetPath, ".git")
		if _, err := os.Stat(gitDir); err != nil {
			res = append(res, CheckResult{
				Category:    "Watchers",
				Name:        w.Name,
				Status:      StatusWarn,
				Message:     fmt.Sprintf("diretório '%s' existe mas não possui repositório Git inicializado", targetPath),
				Remediation: fmt.Sprintf("Inicialize o repositório com: git -C '%s' init", targetPath),
			})
		} else {
			res = append(res, CheckResult{
				Category: "Watchers",
				Name:     w.Name,
				Status:   StatusOK,
				Message:  fmt.Sprintf("diretório e repositório Git válidos (%s)", targetPath),
			})
		}
	}

	return res
}

func renderDoctorReport(results []CheckResult) bool {
	hasFail := false
	currentCategory := ""

	for _, r := range results {
		if r.Category != currentCategory {
			currentCategory = r.Category
			fmt.Printf("[%s]\n", currentCategory)
		}

		icon := "✓"
		switch r.Status {
		case StatusOK:
			icon = "\033[32m[✓ OK]\033[0m"
		case StatusWarn:
			icon = "\033[33m[! WARN]\033[0m"
		case StatusFail:
			icon = "\033[31m[✗ FAIL]\033[0m"
			hasFail = true
		case StatusInfo:
			icon = "\033[36m[i INFO]\033[0m"
		}

		fmt.Printf("  %s %-20s: %s\n", icon, r.Name, r.Message)
		if r.Remediation != "" {
			fmt.Printf("      \033[33m➔ Sugestão: %s\033[0m\n", r.Remediation)
		}
	}

	fmt.Println()
	if hasFail {
		fmt.Println("\033[31m✗ Foram encontrados erros críticos. Siga as sugestões acima para corrigi-los.\033[0m")
	} else {
		fmt.Println("\033[32m✓ Diagnóstico concluído com sucesso! Ambiente pronto para execução do WatchFlow.\033[0m")
	}

	return hasFail
}
