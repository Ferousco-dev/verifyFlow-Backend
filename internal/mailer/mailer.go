// Package mailer sends transactional email behind a small interface. Resend
// is the application provider; the SMTP adapter remains available separately.
package mailer

import (
	"context"
	"log/slog"
)

type Message struct {
	To      string
	Subject string
	Body    string // plain text
}

type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Log is a development-only Sender that prints emails to the log instead of
// sending them. It logs the BODY, which contains live reset links, so main
// only selects it when APP_ENV=development.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email (development log mailer, not sent)", "to", m.To, "subject", m.Subject, "body", m.Body)
	return nil
}
