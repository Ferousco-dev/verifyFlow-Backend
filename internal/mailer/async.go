package mailer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

var (
	ErrQueueFull = errors.New("mailer: queue full")
	ErrClosed    = errors.New("mailer: closed")
)

// Async delivers mail from a bounded queue on background workers, so a slow
// or failing SMTP server neither delays HTTP responses (response time must not
// reveal whether an account exists) nor blocks shutdown indefinitely.
// Send never blocks: when the queue is full it returns ErrQueueFull.
type Async struct {
	next Sender
	log  *slog.Logger

	mu     sync.RWMutex
	closed bool
	ch     chan Message
	wg     sync.WaitGroup
}

func NewAsync(next Sender, log *slog.Logger, workers, queue int) *Async {
	if workers < 1 {
		workers = 1
	}
	if queue < 1 {
		queue = 1
	}
	a := &Async{next: next, log: log, ch: make(chan Message, queue)}
	for i := 0; i < workers; i++ {
		a.wg.Add(1)
		go a.worker()
	}
	return a
}

func (a *Async) worker() {
	defer a.wg.Done()
	for m := range a.ch {
		a.deliver(m)
	}
}

func (a *Async) deliver(m Message) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("mailer panicked", "panic", r)
		}
	}()
	// Detached from the request context: the request is long finished.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.next.Send(ctx, m); err != nil {
		// Never log the body (it may contain a reset link).
		a.log.Error("email delivery failed", "error", err, "subject", m.Subject)
	}
}

func (a *Async) Send(_ context.Context, m Message) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed {
		return ErrClosed
	}
	select {
	case a.ch <- m:
		return nil
	default:
		return ErrQueueFull
	}
}

// Close stops accepting mail and waits for queued mail to be delivered, or
// until ctx expires.
func (a *Async) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.closed {
		a.closed = true
		close(a.ch)
	}
	a.mu.Unlock()

	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
