package logger

import (
	"context"
	"log/slog"
	"regexp"
)

// Padrões de segredos suprimidos antes de qualquer gravação em log ou banco,
// conforme a invariante de supressão de segredos (README). A lista cobre credenciais embutidas em URLs de remote
// (o vetor mais comum em saídas do Git) e tokens de acesso pessoais que podem
// aparecer em mensagens de erro, variáveis de ambiente ou cabeçalhos.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// https://token@host  |  https://user:senha@host
	{regexp.MustCompile(`https?://[^/@\s]+@`), "https://***@"},
	// Tokens do GitHub: ghp_, gho_, ghu_, ghs_, ghr_ e github_pat_
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`), "***"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`), "***"},
	// Cabeçalhos de autorização eventualmente ecoados por ferramentas externas
	{regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)\S+`), "${1}***"},
}

// Redact remove credenciais e tokens conhecidos de um texto arbitrário.
// É aplicada centralmente pelo handler de log, de modo que nenhum call site
// precisa lembrar de sanitizar antes de registrar.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// redactHandler embrulha um slog.Handler aplicando Redact à mensagem e a todos
// os atributos textuais (incluindo os embutidos em erros e grupos) antes de
// delegar a escrita.
type redactHandler struct {
	inner slog.Handler
}

func (h redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cleaned := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		cleaned[i] = redactAttr(a)
	}
	return redactHandler{inner: h.inner.WithAttrs(cleaned)}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()

	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))

	case slog.KindGroup:
		members := v.Group()
		cleaned := make([]slog.Attr, len(members))
		for i, m := range members {
			cleaned[i] = redactAttr(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(cleaned...)}

	case slog.KindAny:
		// Erros carregam com frequência a URL do remote na mensagem
		if err, ok := v.Any().(error); ok {
			if err == nil {
				return slog.Attr{Key: a.Key, Value: slog.AnyValue(nil)}
			}
			return slog.String(a.Key, Redact(err.Error()))
		}
		if s, ok := v.Any().(string); ok {
			return slog.String(a.Key, Redact(s))
		}
	}

	return slog.Attr{Key: a.Key, Value: v}
}
