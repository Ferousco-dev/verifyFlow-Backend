package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // "Name <addr@example.com>" or a bare address
}

// SMTP sends mail over SMTP. Port 465 uses implicit TLS; any other port must
// upgrade with STARTTLS before credentials are sent. Credentials are never
// sent over an unencrypted connection. With no username configured
// (e.g. a local mail catcher) plain SMTP is allowed.
type SMTP struct {
	cfg     SMTPConfig
	timeout time.Duration
	tlsConf *tls.Config
	now     func() time.Time
}

func NewSMTP(cfg SMTPConfig) *SMTP {
	return &SMTP{
		cfg:     cfg,
		timeout: 20 * time.Second,
		tlsConf: &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12},
		now:     time.Now,
	}
}

var errHeaderInjection = errors.New("mailer: header value contains a line break")

func checkHeader(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return errHeaderInjection
	}
	return nil
}

func (s *SMTP) build(m Message) (from, to string, raw []byte, err error) {
	for _, v := range []string{m.To, m.Subject, s.cfg.From} {
		if err := checkHeader(v); err != nil {
			return "", "", nil, err
		}
	}
	fromAddr, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return "", "", nil, fmt.Errorf("mailer: invalid From address: %w", err)
	}
	toAddr, err := mail.ParseAddress(m.To)
	if err != nil {
		return "", "", nil, fmt.Errorf("mailer: invalid recipient: %w", err)
	}

	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", nil, err
	}
	domain := "localhost"
	if i := strings.LastIndex(fromAddr.Address, "@"); i >= 0 {
		domain = fromAddr.Address[i+1:]
	}

	var buf bytes.Buffer
	h := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	h("From", fromAddr.String())
	h("To", toAddr.String())
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", s.now().UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(idBytes)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="UTF-8"`)
	h("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(strings.ReplaceAll(m.Body, "\r\n", "\n"))); err != nil {
		return "", "", nil, err
	}
	if err := qp.Close(); err != nil {
		return "", "", nil, err
	}
	return fromAddr.Address, toAddr.Address, buf.Bytes(), nil
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, to, raw, err := s.build(m)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: s.timeout}
	var conn net.Conn
	if s.cfg.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: s.tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailer: connect: %w", err)
	}
	deadline := s.now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: handshake: %w", err)
	}
	defer c.Close()

	if s.cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tlsConf); err != nil {
				return fmt.Errorf("mailer: starttls: %w", err)
			}
		} else if s.cfg.Username != "" {
			return errors.New("mailer: server does not offer STARTTLS; refusing to send credentials in cleartext")
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("mailer: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: finish: %w", err)
	}
	return c.Quit()
}
