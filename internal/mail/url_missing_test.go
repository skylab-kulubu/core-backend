package mail

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A client built without SKYMAIL_URL sends nothing and says so on every
// attempt, one line each; one left empty for another reason stays quiet.
func TestMailWithoutURLSaysSoOnEveryAttempt(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	missing := &SkyMail{URLMissing: true, TemplateKey: DefaultWelcomeTemplateKey, Tokens: StaticToken("tok"), Logger: log.New(&out, "", 0)}
	if err := missing.Notify(t.Context(), "ticket", "k", Notification{Type: "t"}); !errors.Is(err, ErrTemplateUnconfigured) {
		t.Fatalf("notify: %v", err)
	}
	if got := missing.Welcome(t.Context(), user.User{Email: "ada@example.com"}); got != WelcomeNotSent {
		t.Fatalf("welcome: %v", got)
	}
	missing.Certificate(t.Context(), "ada@example.com", "Ada", nil)
	if got := strings.Count(out.String(), URLMissingLine+"\n"); got != 3 || strings.Count(out.String(), "\n") != 3 {
		t.Fatalf("log %q", out.String())
	}
	if strings.Contains(out.String(), "ada@example.com") {
		t.Fatalf("log names the recipient: %q", out.String())
	}

	var quiet bytes.Buffer
	empty := &SkyMail{Tokens: StaticToken("tok"), Logger: log.New(&quiet, "", 0)}
	_ = empty.Notify(t.Context(), "ticket", "k", Notification{Type: "t"})
	if quiet.Len() != 0 {
		t.Fatalf("quiet client logged %q", quiet.String())
	}
}

func TestURLMissingGauge(t *testing.T) {
	if got := URLMissingPrometheus(); got != "" {
		t.Fatalf("before %q", got)
	}
	if !MarkURLMissing() || MarkURLMissing() {
		t.Fatal("MarkURLMissing is true only the first time")
	}
	if got := URLMissingPrometheus(); got != "# TYPE skylab_skymail_url_missing gauge\nskylab_skymail_url_missing 1\n" {
		t.Fatalf("after %q", got)
	}
}
