package main

import "path/filepath"

// yamlPath prepara um caminho do sistema de arquivos para ser interpolado dentro
// de um escalar YAML entre aspas duplas nas fixtures de teste.
//
// No Windows os caminhos usam '\', que o YAML interpreta como início de escape
// dentro de aspas duplas — "C:\Users\..." falha com "did not find expected
// hexdecimal number" por causa do \U. Converter para barras evita o escape sem
// perder a validade do caminho: o Go aceita '/' no Windows e o ExpandPath
// termina em filepath.Clean, que devolve a forma nativa.
func yamlPath(p string) string {
	return filepath.ToSlash(p)
}
