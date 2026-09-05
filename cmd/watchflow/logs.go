package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/watchflow/watchflow/internal/config"
	"github.com/watchflow/watchflow/internal/logger"
)

var (
	logsFollow    bool
	logsLines     int
	logsLevel     string
	logsComponent string
	logsWatcher   string
	logsJSON      bool
)

// pollInterval é a cadência de verificação de novas linhas em modo --follow.
const logsPollInterval = 400 * time.Millisecond

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Exibe o log estruturado do daemon",
	Long: `Exibe o log JSON gravado pelo daemon em <state_dir>/watchflow.log.

Sob systemd o mesmo fluxo também chega ao journald; este comando é a forma
direta de inspecioná-lo sem depender do journal.`,
	Example: `  watchflow logs                          # últimas 50 linhas
  watchflow logs -n 200                   # últimas 200 linhas
  watchflow logs -f                       # acompanha em tempo real
  watchflow logs --level error            # apenas erros
  watchflow logs --component git -f       # apenas comandos git, ao vivo
  watchflow logs --watcher meu-vault      # apenas um watcher
  watchflow logs --json | jq .            # saída bruta para processamento`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Acompanha o arquivo, exibindo novas linhas conforme surgem")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 50, "Quantidade de linhas finais a exibir")
	logsCmd.Flags().StringVar(&logsLevel, "level", "", "Filtra por nível mínimo: debug, info, warn, error")
	logsCmd.Flags().StringVar(&logsComponent, "component", "", "Filtra por componente (ex.: coordinator, git, pipeline, notify)")
	logsCmd.Flags().StringVar(&logsWatcher, "watcher", "", "Filtra por nome de watcher")
	logsCmd.Flags().BoolVar(&logsJSON, "json", false, "Emite as linhas JSON originais, sem formatação")

	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, _ []string) error {
	logPath, err := resolveLogPath()
	if err != nil {
		return err
	}

	filter, err := newLogFilter()
	if err != nil {
		return err
	}

	f, err := os.Open(logPath) // #nosec G304 -- caminho derivado da configuração do usuário
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("nenhum log encontrado em '%s'; o daemon já foi iniciado alguma vez?", logPath)
		}
		return fmt.Errorf("falha ao abrir o log '%s': %w", logPath, err)
	}
	defer func() { _ = f.Close() }()

	out := cmd.OutOrStdout()

	// Exibe a cauda solicitada e devolve a posição de onde continuar
	offset, err := printTail(out, f, logsLines, filter)
	if err != nil {
		return err
	}

	if !logsFollow {
		return nil
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return followLog(ctx, out, logPath, offset, filter)
}

// resolveLogPath descobre o arquivo de log a partir da configuração vigente.
func resolveLogPath() (string, error) {
	cfgPath := cfgFile
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", fmt.Errorf("falha ao carregar configuração de '%s': %w", cfgPath, err)
	}

	return filepath.Join(config.ExpandPath(cfg.Daemon.StateDir), logger.LogFileName), nil
}

// logFilter decide quais registros são exibidos.
type logFilter struct {
	minLevel  int
	component string
	watcher   string
}

// levelRank ordena os níveis para permitir o filtro por severidade mínima.
var levelRank = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

func newLogFilter() (*logFilter, error) {
	f := &logFilter{
		minLevel:  -1,
		component: strings.TrimSpace(logsComponent),
		watcher:   strings.TrimSpace(logsWatcher),
	}

	if logsLevel != "" {
		lvl, err := logger.ParseLevel(logsLevel)
		if err != nil {
			return nil, err
		}
		rank, ok := levelRank[strings.ToUpper(lvl.String())]
		if !ok {
			return nil, fmt.Errorf("nível de log '%s' não suportado no filtro", logsLevel)
		}
		f.minLevel = rank
	}

	return f, nil
}

// matches avalia uma linha já decodificada. Linhas que não são JSON válido são
// sempre exibidas: podem ser mensagens do runtime que o usuário precisa ver.
func (f *logFilter) matches(entry map[string]any) bool {
	if entry == nil {
		return true
	}

	if f.minLevel >= 0 {
		level, _ := entry["level"].(string)
		if rank, ok := levelRank[strings.ToUpper(level)]; !ok || rank < f.minLevel {
			return false
		}
	}

	if f.component != "" {
		if c, _ := entry["component"].(string); c != f.component {
			return false
		}
	}

	if f.watcher != "" {
		if w, _ := entry["watcher"].(string); w != f.watcher {
			return false
		}
	}

	return true
}

// printTail escreve as últimas n linhas que passam pelo filtro e devolve o
// deslocamento final do arquivo, de onde o modo --follow deve continuar.
func printTail(out io.Writer, f *os.File, n int, filter *logFilter) (int64, error) {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Buffer circular: evita carregar um log grande inteiro em memória
	if n < 0 {
		n = 0
	}
	ring := make([]string, 0, n)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !filter.matches(decodeEntry(line)) {
			continue
		}
		if n == 0 {
			continue
		}
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("falha ao ler o log: %w", err)
	}

	for _, line := range ring {
		writeLine(out, line)
	}

	return f.Seek(0, io.SeekCurrent)
}

// followLog acompanha o arquivo a partir de offset, tratando a rotação por
// tamanho feita pelo próprio daemon: se o arquivo encolher, a leitura recomeça
// do início do novo arquivo em vez de travar num deslocamento inválido.
func followLog(ctx context.Context, out io.Writer, path string, offset int64, filter *logFilter) error {
	ticker := time.NewTicker(logsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		info, err := os.Stat(path)
		if err != nil {
			continue // Rotação em andamento: tenta de novo no próximo ciclo
		}

		if info.Size() < offset {
			offset = 0 // Arquivo rotacionado
		}
		if info.Size() == offset {
			continue
		}

		newOffset, err := drainFrom(out, path, offset, filter)
		if err != nil {
			return err
		}
		offset = newOffset
	}
}

func drainFrom(out io.Writer, path string, offset int64, filter *logFilter) (int64, error) {
	f, err := os.Open(path) // #nosec G304 -- caminho derivado da configuração do usuário
	if err != nil {
		return offset, nil
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, nil
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if filter.matches(decodeEntry(line)) {
			writeLine(out, line)
		}
	}

	return f.Seek(0, io.SeekCurrent)
}

func decodeEntry(line string) map[string]any {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil
	}
	return entry
}

// formatValue evita a notação científica que o %v aplica a números grandes:
// todo número em JSON é decodificado como float64.
func formatValue(v any) string {
	if f, ok := v.(float64); ok {
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

func writeLine(out io.Writer, line string) {
	if logsJSON {
		_, _ = fmt.Fprintln(out, line)
		return
	}
	_, _ = fmt.Fprintln(out, formatEntry(line))
}

// campos exibidos em posição fixa; os demais viram pares chave=valor.
var fixedFields = map[string]bool{
	"time": true, "level": true, "msg": true, "component": true,
}

// formatEntry converte uma linha JSON em texto legível. Linhas que não sejam
// JSON são devolvidas como estão.
func formatEntry(line string) string {
	entry := decodeEntry(line)
	if entry == nil {
		return line
	}

	timestamp := ""
	if raw, ok := entry["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			timestamp = parsed.Format("15:04:05")
		} else {
			timestamp = raw
		}
	}

	level, _ := entry["level"].(string)
	component, _ := entry["component"].(string)
	msg, _ := entry["msg"].(string)

	var extras []string
	for k, v := range entry {
		if fixedFields[k] {
			continue
		}
		extras = append(extras, fmt.Sprintf("%s=%s", k, formatValue(v)))
	}
	sort.Strings(extras)

	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s", timestamp, level)
	if component != "" {
		fmt.Fprintf(&b, " [%s]", component)
	}
	fmt.Fprintf(&b, " %s", msg)
	if len(extras) > 0 {
		fmt.Fprintf(&b, "  %s", strings.Join(extras, " "))
	}

	return b.String()
}
