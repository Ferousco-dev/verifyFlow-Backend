package mailer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSMTP is a minimal plaintext SMTP server that records what it receives.
type fakeSMTP struct {
	ln      net.Listener
	mu      sync.Mutex
	from    string
	rcpts   []string
	data    string
	sawAuth bool
	cmds    []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		f.mu.Lock()
		f.cmds = append(f.cmds, up)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250-fake")
			w("250 8BITMIME") // deliberately no STARTTLS
		case strings.HasPrefix(up, "AUTH"):
			f.mu.Lock()
			f.sawAuth = true
			f.mu.Unlock()
			w("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.from = line[len("MAIL FROM:"):]
			f.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.rcpts = append(f.rcpts, line[len("RCPT TO:"):])
			f.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				sb.WriteString(l)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func testSMTP(t *testing.T, f *fakeSMTP, user string) *SMTP {
	s := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: f.port(), Username: user, Password: "pw", From: "Migo <no-reply@migo.example>"})
	s.timeout = 5 * time.Second
	return s
}

func TestSMTPDeliversWellFormedMessage(t *testing.T) {
	f := newFakeSMTP(t)
	s := testSMTP(t, f, "")
	err := s.Send(context.Background(), Message{
		To: "User <user@example.com>", Subject: "Reset your Migo password — héllo",
		Body: "Open https://app.example.com/reset#token=abc\r\n.\r\nline with a leading dot above",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.from, "no-reply@migo.example") || len(f.rcpts) != 1 || !strings.Contains(f.rcpts[0], "user@example.com") {
		t.Fatalf("envelope wrong: from=%q rcpts=%v", f.from, f.rcpts)
	}
	for _, want := range []string{"From: ", "To: ", "Subject: =?utf-8?q?", "Date: ", "Message-ID: <", "MIME-Version: 1.0",
		"Content-Type: text/plain; charset=\"UTF-8\"", "Content-Transfer-Encoding: quoted-printable", "https://app.example.com/reset#token=3Dabc"} {
		if !strings.Contains(f.data, want) {
			t.Errorf("message missing %q:\n%s", want, f.data)
		}
	}
	if f.sawAuth {
		t.Fatal("no username configured; AUTH must not be attempted")
	}
}

func TestSMTPRefusesCredentialsWithoutTLS(t *testing.T) {
	f := newFakeSMTP(t) // no STARTTLS offered
	s := testSMTP(t, f, "user")
	err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "x", Body: "y"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS refusal, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sawAuth {
		t.Fatal("credentials must never be sent in cleartext")
	}
}

func TestSMTPRejectsHeaderInjectionAndBadAddresses(t *testing.T) {
	f := newFakeSMTP(t)
	s := testSMTP(t, f, "")
	bad := []Message{
		{To: "a@example.com\r\nBcc: victim@example.com", Subject: "x", Body: "y"},
		{To: "a@example.com", Subject: "hi\r\nBcc: victim@example.com", Body: "y"},
		{To: "not-an-address", Subject: "x", Body: "y"},
		{To: "", Subject: "x", Body: "y"},
	}
	for i, m := range bad {
		if err := s.Send(context.Background(), m); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cmds) != 0 {
		t.Fatalf("invalid messages must be rejected before any connection: %v", f.cmds)
	}
}

func TestSMTPConnectionFailure(t *testing.T) {
	s := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "a@example.com"})
	s.timeout = time.Second
	if err := s.Send(context.Background(), Message{To: "b@example.com", Subject: "x", Body: "y"}); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestResendSendsPlainTextEmail(t *testing.T) {
	const apiKey = "re_test_secret"
	type payload struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var got payload
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got.From != "Migo <no-reply@example.com>" || len(got.To) != 1 || got.To[0] != "user@example.com" ||
			got.Subject != "Reset your password" || got.Text != "Open https://app.example.com/#token=secret" {
			t.Errorf("payload = %+v", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"email_123"}`)
	}))
	defer server.Close()

	sender := NewResend(apiKey, "Migo <no-reply@example.com>")
	sender.endpoint = server.URL + "/emails"
	sender.client = server.Client()
	if err := sender.Send(context.Background(), Message{
		To: "user@example.com", Subject: "Reset your password", Body: "Open https://app.example.com/#token=secret",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResendDoesNotExposeAPIErrorBody(t *testing.T) {
	const apiKey = "re_test_secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"`+apiKey+` must not be exposed"}`)
	}))
	defer server.Close()

	sender := NewResend(apiKey, "no-reply@example.com")
	sender.endpoint = server.URL
	sender.client = server.Client()
	err := sender.Send(context.Background(), Message{To: "user@example.com", Subject: "Test", Body: "text"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP status error, got %v", err)
	}
	if strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), "must not be exposed") {
		t.Fatalf("Resend response details leaked in error: %v", err)
	}
}

func TestResendRejectsHeaderInjectionBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	sender := NewResend("re_test_secret", "no-reply@example.com")
	sender.endpoint = server.URL
	sender.client = server.Client()
	if err := sender.Send(context.Background(), Message{To: "user@example.com", Subject: "Hello\r\nBcc: victim@example.com", Body: "text"}); err == nil {
		t.Fatal("expected injected header to be rejected")
	}
	if called {
		t.Fatal("invalid message reached the Resend API")
	}
}

// ---- Async ----

type recorder struct {
	mu   sync.Mutex
	msgs []Message
	fn   func(Message) error
}

func (r *recorder) Send(_ context.Context, m Message) error {
	if r.fn != nil {
		if err := r.fn(m); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return nil
}

func TestAsyncDeliversAndDrainsOnClose(t *testing.T) {
	rec := &recorder{}
	a := NewAsync(rec, quietLog, 2, 50)
	for i := 0; i < 20; i++ {
		if err := a.Send(context.Background(), Message{To: "a@example.com", Subject: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 20 {
		t.Fatalf("delivered %d/20", len(rec.msgs))
	}
	if err := a.Send(context.Background(), Message{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
	if err := a.Close(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestAsyncNeverBlocksWhenFull(t *testing.T) {
	release := make(chan struct{})
	rec := &recorder{fn: func(Message) error { <-release; return nil }}
	a := NewAsync(rec, quietLog, 1, 2)

	full := 0
	start := time.Now()
	for i := 0; i < 10; i++ {
		if err := a.Send(context.Background(), Message{To: "a@example.com"}); errors.Is(err, ErrQueueFull) {
			full++
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("Send blocked")
	}
	if full == 0 {
		t.Fatal("expected ErrQueueFull once the bounded queue filled")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.Close(ctx)
}

func TestAsyncSurvivesFailuresAndPanics(t *testing.T) {
	var n atomic.Int32
	rec := &recorder{fn: func(m Message) error {
		switch m.Subject {
		case "panic":
			panic("boom")
		case "fail":
			return errors.New("smtp down")
		}
		n.Add(1)
		return nil
	}}
	a := NewAsync(rec, quietLog, 1, 10)
	for _, s := range []string{"panic", "fail", "ok", "ok"} {
		_ = a.Send(context.Background(), Message{Subject: s})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("worker died after a failure/panic; delivered %d/2", n.Load())
	}
}

func TestAsyncCloseHonoursContext(t *testing.T) {
	block := make(chan struct{})
	rec := &recorder{fn: func(Message) error { <-block; return nil }}
	a := NewAsync(rec, quietLog, 1, 2)
	_ = a.Send(context.Background(), Message{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close must give up when ctx expires, got %v", err)
	}
	close(block)
}
