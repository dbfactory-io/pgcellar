package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

// SignatureHeader carries "sha256=<hex hmac of the body>" when a secret is set.
const SignatureHeader = "X-PgCellar-Signature"

type webhook struct {
	cfg config.WebhookAlert
	o   Options
}

func (w *webhook) Name() string { return Webhook }

func (w *webhook) Target() string { return w.cfg.URL }

func (w *webhook) Env() []EnvStatus { return envStatus(w.o, w.cfg.SecretEnv) }

// Payload is the JSON body of the webhook.
type Payload struct {
	Message
	Text string `json:"text"` // the plain-text rendering, ready to show
}

// Sign returns the signature header value for a body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (w *webhook) Send(ctx context.Context, m Message) error {
	var secret string
	if w.cfg.SecretEnv != "" {
		var err error
		if secret, err = needEnv(w.o, w.cfg.SecretEnv); err != nil {
			return err
		}
	}
	body, err := json.Marshal(Payload{Message: m, Text: m.Text()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pgcellar")
	if secret != "" {
		req.Header.Set(SignatureHeader, Sign(secret, body))
	}
	resp, err := w.o.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the webhook answered %d", resp.StatusCode)
	}
	return nil
}
