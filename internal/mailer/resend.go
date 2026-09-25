package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

const resendEmailsURL = "https://api.resend.com/emails"

type Resend struct {
	apiKey   string
	from     string
	client   *http.Client
	endpoint string
}

func NewResend(apiKey, from string) *Resend {
	return &Resend{
		apiKey:   apiKey,
		from:     from,
		client:   &http.Client{Timeout: 10 * time.Second},
		endpoint: resendEmailsURL,
	}
}

func (r *Resend) Send(ctx context.Context, m Message) error {
	for _, value := range []string{r.from, m.To, m.Subject} {
		if err := checkHeader(value); err != nil {
			return err
		}
	}
	if _, err := mail.ParseAddress(r.from); err != nil {
		return fmt.Errorf("mailer: invalid Resend sender address: %w", err)
	}
	if _, err := mail.ParseAddress(m.To); err != nil {
		return fmt.Errorf("mailer: invalid recipient: %w", err)
	}
	if strings.TrimSpace(r.apiKey) == "" {
		return errors.New("mailer: Resend API key is required")
	}

	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}{From: r.from, To: []string{m.To}, Subject: m.Subject, Text: m.Body})
	if err != nil {
		return fmt.Errorf("mailer: encode Resend email: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mailer: create Resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("mailer: Resend request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("mailer: Resend API returned HTTP %d", resp.StatusCode)
	}
	return nil
}
