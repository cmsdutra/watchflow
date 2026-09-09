package integration_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watchflow/watchflow/tests/testutil"
)

// Regressão do risco de fim de linha em cofres compartilhados entre Windows e
// Linux (ver README, "Arquivos que conflitam sempre" e "Limitações conhecidas").
//
// O Git for Windows usa core.autocrlf=true por padrão. Num cofre compartilhado
// com Linux isso torna o conteúdo do blob dependente da máquina que criou o
// arquivo. A mitigação é um '.gitattributes' versionado com '* text=auto
// eol=lf' — e o que estes testes travam é que a mitigação de fato neutraliza a
// conversão, e que o WatchFlow nunca dispara a reescrita em massa.
//
// Roda nas duas plataformas: core.autocrlf é configuração do repositório, então
// o cenário do Windows é reproduzível num runner Linux, que é o que permite
// pegar a regressão sem depender de uma máquina Windows.

// gitattributesLF é a mitigação recomendada no README e no plano.
const gitattributesLF = "* text=auto eol=lf\n"

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s falhou em %s: %v (%s)", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitattributesNeutralizaAutocrlf prova que, com a mitigação versionada, um
// clone configurado como o Git forWindows padrão não reescreve o arquivo em
// disco — nem, por consequência, faz o watcher enxergar o cofre inteiro
// modificado.
func TestGitattributesNeutralizaAutocrlf(t *testing.T) {
	origem := testutil.NewGitSandbox(t)
	// Conteúdo publicado por uma máquina Linux: LF puro.
	origem.WriteFile(".gitattributes", gitattributesLF)
	origem.WriteFile("nota.md", "linha um\nlinha dois\nlinha tres\n")
	origem.CommitAll("cofre publicado do Linux")

	clone := testutil.CloneRepo(t, origem.RootDir)
	// A máquina Windows com o padrão do Git for Windows.
	gitIn(t, clone.RootDir, "config", "core.autocrlf", "true")

	// Força o checkout a reaplicar os filtros, como faria um clone novo já
	// nessa configuração.
	notaPath := filepath.Join(clone.RootDir, "nota.md")
	if err := os.Remove(notaPath); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone.RootDir, "checkout", "--", "nota.md")

	conteudo, err := os.ReadFile(notaPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(conteudo, []byte("\r\n")) {
		t.Error("'.gitattributes' com eol=lf não impediu a conversão para CRLF no checkout")
	}

	if sujo := gitIn(t, clone.RootDir, "status", "--porcelain"); sujo != "" {
		t.Errorf("checkout deixou a árvore suja, o que faria o watcher commitar o cofre inteiro:\n%s", sujo)
	}
}

// TestAdicionarGitattributesNaoReescreveOCofre delimita o risco que o plano
// classificava como "commit gigante automático".
//
// Adotar a mitigação num cofre já existente não modifica arquivo nenhum: a
// reescrita em massa só acontece sob um 'git add --renormalize' explícito, que
// o WatchFlow nunca executa. O teste trava as duas metades desse fato, porque
// é a segunda que justifica o custo de manter a primeira.
func TestAdicionarGitattributesNaoReescreveOCofre(t *testing.T) {
	sb := testutil.NewGitSandbox(t)

	// Cofre criado por uma máquina Windows "ingênua": CRLF entrou no blob.
	for _, nome := range []string{"a.md", "b.md", "c.md"} {
		sb.WriteFile(nome, "linha um\r\nlinha dois\r\n")
	}
	sb.CommitAll("cofre com CRLF no histórico")

	sb.WriteFile(".gitattributes", gitattributesLF)
	sb.CommitAll("adota a mitigação de fim de linha")

	if sujo := gitIn(t, sb.RootDir, "status", "--porcelain"); sujo != "" {
		t.Errorf("adicionar o '.gitattributes' modificou arquivos por conta própria:\n%s", sujo)
	}

	// A renormalização é deliberada e manual — e é a única coisa que reescreve
	// o cofre. Se um dia o pipeline passar a executá-la, o teste acima continua
	// verde e este documenta o porquê de ele importar.
	gitIn(t, sb.RootDir, "add", "--renormalize", ".")
	staged := gitIn(t, sb.RootDir, "status", "--porcelain")
	if staged == "" {
		t.Skip("Git desta máquina não renormalizou; nada a comparar")
	}
	if n := len(strings.Split(staged, "\n")); n != 3 {
		t.Errorf("esperava a renormalização explícita tocar os 3 arquivos, tocou %d:\n%s", n, staged)
	}
}
