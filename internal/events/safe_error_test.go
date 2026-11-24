package events

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSafeErrorTextMasks(t *testing.T) {
	pg := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "ada@example.com"`}
	for err, want := range map[error]string{
		fmt.Errorf("reading the Events: %w", pg):       "reading the Events: PostgreSQL error (SQLSTATE 22P02)",
		errors.New("dial 10.0.1.5:5432 for ada@x.org"): "dial [ip]:5432 for [e-mail]",
	} {
		if got := SafeErrorText(err); got != want {
			t.Fatalf("%q, want %q", got, want)
		}
	}
	if SafeErrorText(nil) != "" {
		t.Fatal("nil")
	}
}
