package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

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
	Long:  `Inspeciona limites de inotify do kernel Linux, versão do Git, permissões de disco, integridade da base SQLite e estado do daemon.`,
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
	cfg, _ := config.Load(cfgPath)

	var results []CheckResult

	// 1. Limites inotify do Kernel (Linux)
	results = append(results, checkInotifyLimits()...)

	// 2. Binário e Versão do Git
	results = append(results, checkGitRequirement())

	// 3. Diretório de Estado e SQLite
	stateDir := "~/.local/state/watchflow"
	if cfg != nil && cfg.Daemon.StateDir != "" {
		stateDir = cfg.Daemon.StateDir
	}
	results = append(results, checkStateDirectory(stateDir)...)

	// 4. Socket IPC e Daemon
	sockPath := resolveSocketPath()
	results = append(results, checkIPCSocket(sockPath))

	// 5. Repositórios dos Watchers configurados
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
