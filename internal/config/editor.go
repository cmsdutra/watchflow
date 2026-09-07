package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultIgnorePatterns é a lista de exclusão usada por 'config add-watcher'
// quando nenhum --ignore é informado. Espelha o que os demais watchers do
// config.yaml de exemplo já declaram.
var DefaultIgnorePatterns = []string{
	".git/**",
	".obsidian/cache/**",
	".obsidian/workspace*",
	"**/*.tmp",
	"**/.DS_Store",
	".tmp/**",
}

// AddWatcherToFile insere um novo watcher no arquivo YAML em filePath e
// grava o resultado. A edição opera em cima da árvore de nós do yaml.v3 (não
// da struct Config), o que preserva os comentários do restante do arquivo —
// o config.yaml de referência do WatchFlow é fortemente comentado, e um
// round-trip via struct+Marshal destruiria essa documentação. O reencode
// ainda pode remover linhas em branco entre seções e reajustar o espaçamento
// de comentários à direita; é uma limitação conhecida do encoder de nós do
// yaml.v3, não uma perda de conteúdo.
func AddWatcherToFile(filePath string, w WatcherConfig) error {
	expanded := ExpandPath(filePath)
	data, err := os.ReadFile(expanded)
	if err != nil {
		return fmt.Errorf("falha ao ler arquivo de configuração '%s': %w", expanded, err)
	}

	// DefaultPipelineName só existe de fato quando o próprio arquivo não
	// declara nenhum pipeline para o watcher: nesse caso applyDefaults injeta
	// 'default' em memória a cada Load(). Como aqui estamos escrevendo o nome
	// do pipeline explicitamente no YAML, essa injeção implícita não ocorre —
	// então é preciso apontar para um pipeline que realmente existe no
	// arquivo antes de gravar.
	if len(w.Pipelines) == 0 {
		resolved, err := resolveDefaultPipeline(data)
		if err != nil {
			return err
		}
		w.Pipelines = []string{resolved}
	}

	newData, err := AddWatcherYAML(data, w)
	if err != nil {
		return err
	}

	if _, err := LoadBytes(newData, true); err != nil {
		return fmt.Errorf("a configuração resultante seria inválida: %w", err)
	}

	return atomicWriteFile(expanded, newData)
}

// resolveDefaultPipeline decide qual pipeline usar para um watcher novo
// quando nenhum --pipeline foi informado: o embutido 'default' se ele já
// existir declarado no arquivo, o único pipeline existente se houver apenas
// um, ou erro pedindo para o usuário escolher explicitamente.
func resolveDefaultPipeline(data []byte) (string, error) {
	existing, err := LoadBytes(data, false)
	if err != nil {
		return "", fmt.Errorf("configuração atual é inválida; não é possível inferir o pipeline padrão: %w", err)
	}

	if _, ok := existing.Pipelines[DefaultPipelineName]; ok {
		return DefaultPipelineName, nil
	}
	if len(existing.Pipelines) == 1 {
		for name := range existing.Pipelines {
			return name, nil
		}
	}

	names := make([]string, 0, len(existing.Pipelines))
	for name := range existing.Pipelines {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", fmt.Errorf("nenhum --pipeline informado e não há um único pipeline óbvio; use --pipeline com um dos disponíveis: %s", strings.Join(names, ", "))
}

// RemoveWatcherFromFile exclui o watcher de nome dado do arquivo YAML em
// filePath, com a mesma preservação de comentários de AddWatcherToFile.
func RemoveWatcherFromFile(filePath string, name string) error {
	expanded := ExpandPath(filePath)
	data, err := os.ReadFile(expanded)
	if err != nil {
		return fmt.Errorf("falha ao ler arquivo de configuração '%s': %w", expanded, err)
	}

	newData, err := RemoveWatcherYAML(data, name)
	if err != nil {
		return err
	}

	if _, err := LoadBytes(newData, true); err != nil {
		return fmt.Errorf("a configuração resultante seria inválida: %w", err)
	}

	return atomicWriteFile(expanded, newData)
}

// AddWatcherYAML devolve o YAML de data com um novo watcher anexado à seção
// 'watchers'. Falha se já existir um watcher com o mesmo nome.
func AddWatcherYAML(data []byte, w WatcherConfig) ([]byte, error) {
	if w.Name == "" {
		return nil, fmt.Errorf("nome do watcher é obrigatório")
	}
	if w.Path == "" {
		return nil, fmt.Errorf("caminho do watcher é obrigatório")
	}

	root, watchersNode, err := parseWatchersNode(data)
	if err != nil {
		return nil, err
	}

	for _, item := range watchersNode.Content {
		if nameVal := mappingValue(item, "name"); nameVal != nil && nameVal.Value == w.Name {
			return nil, fmt.Errorf("já existe um watcher chamado '%s'", w.Name)
		}
	}

	watchersNode.Content = append(watchersNode.Content, watcherToNode(w))

	return encodeNode(root)
}

// RemoveWatcherYAML devolve o YAML de data sem o watcher de nome dado. Falha
// se nenhum watcher com esse nome existir.
func RemoveWatcherYAML(data []byte, name string) ([]byte, error) {
	root, watchersNode, err := parseWatchersNode(data)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, item := range watchersNode.Content {
		if nameVal := mappingValue(item, "name"); nameVal != nil && nameVal.Value == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, fmt.Errorf("nenhum watcher chamado '%s' encontrado", name)
	}

	watchersNode.Content = append(watchersNode.Content[:idx], watchersNode.Content[idx+1:]...)

	return encodeNode(root)
}

// parseWatchersNode decodifica data em uma árvore de nós e localiza a
// sequência 'watchers' no mapa raiz.
func parseWatchersNode(data []byte) (root *yaml.Node, watchers *yaml.Node, err error) {
	root = &yaml.Node{}
	if err := yaml.Unmarshal(data, root); err != nil {
		return nil, nil, fmt.Errorf("sintaxe YAML inválida: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("estrutura de configuração inesperada: raiz não é um mapa")
	}

	docMap := root.Content[0]
	watchersNode := mappingValue(docMap, "watchers")
	if watchersNode == nil || watchersNode.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("seção 'watchers' não encontrada ou não é uma lista")
	}

	return root, watchersNode, nil
}

// mappingValue busca o nó de valor associado a key dentro de um nó de mapa.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func watcherToNode(w WatcherConfig) *yaml.Node {
	debounce := w.Debounce
	if debounce == "" {
		debounce = "15s"
	}
	maxWait := w.MaxWait
	if maxWait == "" {
		maxWait = "60s"
	}
	pullInterval := w.PullInterval
	if pullInterval == "" {
		pullInterval = DefaultPullInterval
	}
	pipelines := w.Pipelines
	if len(pipelines) == 0 {
		pipelines = []string{DefaultPipelineName}
	}

	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	putStr := func(key, val string) {
		m.Content = append(m.Content, plainKey(key), quotedStr(val))
	}

	putStr("name", w.Name)
	putStr("path", w.Path)
	if w.Enabled != nil && !*w.Enabled {
		m.Content = append(m.Content, plainKey("enabled"), boolNode(false))
	}
	putStr("debounce", debounce)
	putStr("max_wait", maxWait)
	putStr("pull_interval", pullInterval)

	if len(w.Ignore) > 0 {
		m.Content = append(m.Content, plainKey("ignore"), strSeqNode(w.Ignore))
	}
	m.Content = append(m.Content, plainKey("pipelines"), strSeqNode(pipelines))

	return m
}

func plainKey(k string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
}

func quotedStr(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func boolNode(v bool) *yaml.Node {
	val := "false"
	if v {
		val = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: val}
}

func strSeqNode(items []string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, it := range items {
		seq.Content = append(seq.Content, quotedStr(it))
	}
	return seq
}

func encodeNode(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("falha ao serializar configuração: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("falha ao finalizar serialização da configuração: %w", err)
	}
	return buf.Bytes(), nil
}

// atomicWriteFile grava data em path por meio de um arquivo temporário no
// mesmo diretório seguido de rename, para que um processo lendo o config
// concorrentemente (ex.: 'watchflow reload') nunca veja um arquivo truncado.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".watchflow-config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("falha ao criar arquivo temporário: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("falha ao escrever configuração temporária: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("falha ao fechar configuração temporária: %w", err)
	}

	mode := os.FileMode(0644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode()
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("falha ao ajustar permissões da configuração: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("falha ao substituir arquivo de configuração: %w", err)
	}
	return nil
}
