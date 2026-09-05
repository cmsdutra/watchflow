package logger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Entry é um registro do log JSON já decodificado, com os campos fixos
// separados dos atributos livres.
type Entry struct {
	Time      time.Time
	Level     string
	Component string
	Message   string
	Attrs     map[string]any

	// Raw preserva a linha original, útil para exibição sem formatação.
	Raw string
}

// Attr devolve o valor textual de um atributo, ou vazio se ausente.
func (e Entry) Attr(key string) string {
	v, ok := e.Attrs[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// AttrKeys devolve os nomes dos atributos em ordem estável.
func (e Entry) AttrKeys() []string {
	keys := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fixedEntryFields são os campos com posição própria em Entry.
var fixedEntryFields = map[string]bool{
	"time": true, "level": true, "msg": true, "component": true,
}

// ParseEntry decodifica uma linha do log JSON. Retorna ok=false para linhas que
// não sejam JSON válido, que o chamador pode exibir como texto cru.
func ParseEntry(line string) (Entry, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return Entry{Raw: line}, false
	}

	e := Entry{Raw: line, Attrs: make(map[string]any)}

	if s, ok := raw["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			// O log é gravado em UTC; a exibição usa o fuso local do usuário.
			e.Time = parsed.Local()
		}
	}
	e.Level, _ = raw["level"].(string)
	e.Component, _ = raw["component"].(string)
	e.Message, _ = raw["msg"].(string)

	for k, v := range raw {
		if !fixedEntryFields[k] {
			e.Attrs[k] = v
		}
	}

	return e, true
}

// Tail lê as últimas n entradas válidas do arquivo de log.
//
// Usa um buffer circular para não carregar em memória um log que pode ter
// megabytes, e ignora silenciosamente linhas malformadas — a leitura da cauda
// é para exibição, não para auditoria estrita.
func Tail(path string, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}

	f, err := os.Open(path) // #nosec G304 -- caminho derivado da configuração
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	ring := make([]Entry, 0, n)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		entry, ok := ParseEntry(line)
		if !ok {
			continue
		}

		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("falha ao ler o log '%s': %w", path, err)
	}

	return ring, nil
}
