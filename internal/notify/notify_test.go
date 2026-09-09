package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingNotifier captura o que foi entregue ao backend.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []Notification
}

func (r *recordingNotifier) Notify(_ context.Context, n Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
	return nil
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func newTestDispatcher(opts Options) (*dispatcher, *recordingNotifier) {
	rec := &recordingNotifier{}
	d := New(opts).(*dispatcher)
	d.backend = rec
	return d, rec
}

func TestGatesRespectConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		kind    Kind
		wantOut bool
	}{
		{"conflito habilitado", Options{Enabled: true, OnConflict: true}, KindConflict, true},
		{"conflito desabilitado", Options{Enabled: true, OnConflict: false}, KindConflict, false},
		{"erro habilitado", Options{Enabled: true, OnError: true}, KindError, true},
		{"sucesso silencioso por padrão", Options{Enabled: true, OnError: true}, KindSuccess, false},
		{"sucesso habilitado", Options{Enabled: true, OnSuccess: true}, KindSuccess, true},
		{"tudo desligado", Options{Enabled: false, OnConflict: true}, KindConflict, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, rec := newTestDispatcher(tc.opts)

			if err := d.Notify(context.Background(), Notification{Kind: tc.kind, Watcher: "vault"}); err != nil {
				t.Fatalf("Notify retornou erro: %v", err)
			}

			got := rec.count() == 1
			if got != tc.wantOut {
				t.Errorf("entrega = %v, esperado %v", got, tc.wantOut)
			}
		})
	}
}

// TestNotifyOutlivesCancelledContext garante que o alerta de conflito é enviado
// mesmo quando o contexto do job já foi cancelado — o caso do encerramento do
// daemon logo após detectar o conflito.
func TestNotifyOutlivesCancelledContext(t *testing.T) {
	d, rec := newTestDispatcher(Options{Enabled: true, OnConflict: true})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.Notify(ctx, Conflict("vault", "detalhe")); err != nil {
		t.Fatalf("Notify retornou erro: %v", err)
	}
	if rec.count() != 1 {
		t.Error("notificação de conflito foi descartada por contexto já cancelado")
	}
}

func TestConflictMessageIsActionable(t *testing.T) {
	n := Conflict("meu-vault", "CONFLICT (content): Merge conflict in nota.md")

	if n.Kind != KindConflict {
		t.Errorf("kind = %s", n.Kind)
	}
	if !strings.Contains(n.Message, "watchflow resume meu-vault") {
		t.Errorf("mensagem não instrui o usuário sobre como retomar: %s", n.Message)
	}
	if !strings.Contains(n.Message, "restaurada intacta") {
		t.Errorf("mensagem deveria tranquilizar sobre a integridade da árvore: %s", n.Message)
	}
	if n.Detail == "" {
		t.Error("detalhe técnico do conflito foi perdido")
	}
}

func TestDesktopUrgencyForConflictIsCritical(t *testing.T) {
	var gotArgs []string

	d := &desktopNotifier{
		binary: "notify-send",
		run: func(_ context.Context, _ string, args ...string) error {
			gotArgs = args
			return nil
		},
	}

	if err := d.Notify(context.Background(), Conflict("vault", "detalhe do conflito")); err != nil {
		t.Fatalf("Notify falhou: %v", err)
	}

	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--urgency=critical") {
		t.Errorf("conflito deveria usar urgência critical: %v", gotArgs)
	}
	if !strings.Contains(joined, "--expire-time=0") {
		t.Errorf("alerta de conflito não pode expirar sozinho: %v", gotArgs)
	}
	if !strings.Contains(joined, "--app-name=WatchFlow") {
		t.Errorf("esperava app-name definido: %v", gotArgs)
	}

	// Invariante "sem shell" (README): argumentos como slice, nunca interpolados em shell
	for _, a := range gotArgs {
		if strings.HasPrefix(a, "-c") || a == "sh" || a == "bash" {
			t.Errorf("argumento suspeito de invocação por shell: %q", a)
		}
	}
}

func TestDesktopSuccessIsLowUrgency(t *testing.T) {
	var gotArgs []string
	d := &desktopNotifier{
		binary: "notify-send",
		run: func(_ context.Context, _ string, args ...string) error {
			gotArgs = args
			return nil
		},
	}

	if err := d.Notify(context.Background(), Success("vault", 3, 2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--urgency=low") {
		t.Errorf("sucesso deveria ser de baixa urgência: %v", gotArgs)
	}
}

func TestWebhookDeliversJSON(t *testing.T) {
	var (
		mu       sync.Mutex
		received map[string]any
		gotType  string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wn, err := newWebhookNotifier(srv.URL)
	if err != nil {
		t.Fatalf("falha ao criar webhook: %v", err)
	}

	if err := wn.Notify(context.Background(), Conflict("vault", "detalhe")); err != nil {
		t.Fatalf("envio falhou: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotType != "application/json" {
		t.Errorf("content-type = %q", gotType)
	}
	if received["kind"] != "conflict" {
		t.Errorf("kind = %v", received["kind"])
	}
	if received["watcher"] != "vault" {
		t.Errorf("watcher = %v", received["watcher"])
	}
	if received["source"] != "watchflow" {
		t.Errorf("source = %v", received["source"])
	}
}

func TestWebhookReportsNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wn, err := newWebhookNotifier(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := wn.Notify(context.Background(), Success("v", 1, time.Second)); err == nil {
		t.Error("esperava erro para resposta 500")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	invalid := []string{"", "   ", "ftp://host/x", "não é url", "https://"}
	for _, u := range invalid {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("esperava erro para webhook_url %q", u)
		}
	}
	for _, u := range []string{"http://localhost:9000/hook", "https://example.com/wf"} {
		if err := ValidateWebhookURL(u); err != nil {
			t.Errorf("webhook_url %q deveria ser aceita: %v", u, err)
		}
	}
}

// TestUnavailableBackendFallsBackToLog garante que uma configuração de webhook
// inválida não deixa o usuário sem nenhum alerta de conflito.
func TestUnavailableBackendFallsBackToLog(t *testing.T) {
	n := New(Options{Enabled: true, OnConflict: true, Backend: "webhook", WebhookURL: "não-é-url"})

	d, ok := n.(*dispatcher)
	if !ok {
		t.Fatalf("tipo inesperado: %T", n)
	}
	if _, isLog := d.backend.(*logNotifier); !isLog {
		t.Errorf("esperava fallback para logNotifier, obteve %T", d.backend)
	}
	if err := d.Notify(context.Background(), Conflict("v", "d")); err != nil {
		t.Errorf("fallback deveria entregar sem erro: %v", err)
	}
}

func TestDisabledUsesNoop(t *testing.T) {
	n := New(Options{Enabled: false, Backend: "desktop"})
	d := n.(*dispatcher)
	if _, isNoop := d.backend.(noopNotifier); !isNoop {
		t.Errorf("esperava noopNotifier quando desabilitado, obteve %T", d.backend)
	}
}

// TestRepeatedAlertsAreSuppressed cobre a regressão observada em teste de ponta
// a ponta: um único conflito de merge gerava um alerta por ciclo de reavaliação
// (11 em ~5s), cada um um popup crítico que não expira sozinho.
func TestRepeatedAlertsAreSuppressed(t *testing.T) {
	d, rec := newTestDispatcher(Options{Enabled: true, OnConflict: true, OnError: true})

	for i := 0; i < 12; i++ {
		if err := d.Notify(context.Background(), Conflict("vault", "detalhe")); err != nil {
			t.Fatal(err)
		}
	}

	if rec.count() != 1 {
		t.Errorf("esperava 1 alerta para a mesma condição persistente, obteve %d", rec.count())
	}

	// Watcher distinto é uma condição distinta e deve alertar
	if err := d.Notify(context.Background(), Conflict("outro-vault", "detalhe")); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Errorf("alerta de outro watcher foi suprimido indevidamente: %d", rec.count())
	}

	// Tipo distinto para o mesmo watcher também deve passar
	if err := d.Notify(context.Background(), Failure("vault", "pipe", "detalhe")); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 3 {
		t.Errorf("alerta de tipo distinto foi suprimido indevidamente: %d", rec.count())
	}
}

func TestResetReopensAlertWindow(t *testing.T) {
	d, rec := newTestDispatcher(Options{Enabled: true, OnConflict: true})

	_ = d.Notify(context.Background(), Conflict("vault", "d"))
	_ = d.Notify(context.Background(), Conflict("vault", "d"))
	if rec.count() != 1 {
		t.Fatalf("esperava 1 alerta antes do reset, obteve %d", rec.count())
	}

	// 'watchflow resume' sinaliza que o usuário tratou o problema
	d.Reset("vault")

	_ = d.Notify(context.Background(), Conflict("vault", "d"))
	if rec.count() != 2 {
		t.Errorf("após resume, um novo conflito deveria alertar novamente: %d", rec.count())
	}
}

func TestSuccessIsNeverDeduplicated(t *testing.T) {
	d, rec := newTestDispatcher(Options{Enabled: true, OnSuccess: true})

	for i := 0; i < 5; i++ {
		_ = d.Notify(context.Background(), Success("vault", 1, time.Second))
	}
	if rec.count() != 5 {
		t.Errorf("cada sincronização é um evento distinto; esperava 5, obteve %d", rec.count())
	}
}
