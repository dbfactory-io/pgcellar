package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

type telegram struct {
	cfg config.TelegramAlert
	o   Options
}

func (t *telegram) Name() string { return Telegram }

func (t *telegram) Target() string { return "chat " + t.cfg.ChatID }

func (t *telegram) Env() []EnvStatus { return envStatus(t.o, t.cfg.TokenEnv) }

func (t *telegram) Send(ctx context.Context, m Message) error {
	token, err := needEnv(t.o, t.cfg.TokenEnv)
	if err != nil {
		return err
	}
	base := strings.TrimRight(t.o.TelegramBase, "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  t.cfg.ChatID,
		"text":                     m.HTML(),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New(scrub(err.Error(), token))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.o.client().Do(req)
	if err != nil {
		return errors.New(scrub(err.Error(), token))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 != 2 || !out.OK {
		why := out.Description
		if why == "" {
			why = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("the Telegram API answered %d: %s", resp.StatusCode, scrub(why, token))
	}
	return nil
}
