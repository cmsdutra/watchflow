package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// webhookNotifier entrega o alerta como JSON via POST para um endpoint HTTP.
type webhookNotifier struct {
	url    string
	client *http.Client
}

// ValidateWebhookURL verifica se a URL de webhook é utilizável. Exposta para
// que 'watchflow config validate' reprove a configuração antes do boot.
func ValidateWebhookURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("campo 'webhook_url' é obrigatório quando backend é 'webhook'")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("webhook_url inválida: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("webhook_url deve usar esquema http ou https (recebido: '%s')", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("webhook_url não contém host")
	}

	return nil
}

func newWebhookNotifier(rawURL string) (*webhookNotifier, error) {
	if err := ValidateWebhookURL(rawURL); err != nil {
		return nil, err
	}

	return &webhookNotifier{
		url:    strings.TrimSpace(rawURL),
		client: &http.Client{Timeout: sendTimeout},
	}, nil
}

func (w *webhookNotifier) Notify(ctx context.Context, n Notification) error {
	payload, err := json.Marshal(struct {
		Notification
		Timestamp string `json:"timestamp"`
		Source    string `json:"source"`
	}{
		Notification: n,
		Timestamp:    time.Now().Format(time.RFC3339),
		Source:       "watchflow",
	})
	if err != nil {
		return fmt.Errorf("falha ao serializar notificação: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("falha ao montar requisição do webhook: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "watchflow")

	resp, err := w.client.Do(req)
	if err != nil {
		// O erro do net/http embute a URL, que pode conter token: a supressão
		// central do logger cuida disso no momento da gravação.
		return fmt.Errorf("falha ao enviar webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook respondeu com status %d", resp.StatusCode)
	}

	return nil
}
