package consent

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
)

type blockingMailer struct {
	started chan struct{}
	release chan struct{}
	sent    chan string
}

func (m *blockingMailer) ConsentConfirmation(_ context.Context, _, recipient string, _ map[string]string) {
	m.started <- struct{}{}
	<-m.release
	m.sent <- recipient
}

// A confirmation mail goes after the request that recorded it was
// answered, while the row already says it was sent. Shutdown waits on Idle,
// so a mail that is going is not cut off with the process.
func TestIdleClosesOnlyWhenNoConfirmationMailIsGoing(t *testing.T) {
	t.Parallel()
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	config, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://api.example.test", TextURLEnv: "https://a.test/acik-riza"}))
	if err != nil {
		t.Fatal(err)
	}
	mailer := &blockingMailer{started: make(chan struct{}, 1), release: make(chan struct{}), sent: make(chan string, 1)}
	s := NewService(nil, config, mailer)

	select {
	case <-s.Idle():
	default:
		t.Fatal("Idle is open with no mail going")
	}

	s.sendConfirmation(uuid.New(), Grant{Purpose: PurposeEventInvitations, Email: "a@example.com"})
	<-mailer.started
	idle := s.Idle()
	select {
	case <-idle:
		t.Fatal("Idle closed while a mail was going")
	case <-time.After(50 * time.Millisecond):
	}
	close(mailer.release)
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("Idle did not close after the mail went")
	}
	if got := <-mailer.sent; got != "a@example.com" {
		t.Fatalf("sent to %q", got)
	}
	var none *Service
	select {
	case <-none.Idle():
	default:
		t.Fatal("a nil service is not idle")
	}
}
