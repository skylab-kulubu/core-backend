package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/consent"
)

func TestConsentWithdrawPurposes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []consent.Purpose
		code int
	}{
		{[]string{"-purpose", "promotional"}, []consent.Purpose{consent.PurposePromotional}, 0},
		{[]string{"-purpose", " promotional , event_invitations "}, []consent.Purpose{consent.PurposePromotional, consent.PurposeEventInvitations}, 0},
		{nil, nil, 2},
		{[]string{"-purpose", ","}, nil, 2},
		{[]string{"-purpose", "promotional,promotional"}, nil, 2},
		{[]string{"-purpose", "recruitment_pool"}, nil, 2},
		{[]string{"-purpose", "newsletter"}, nil, 2},
		// The address never comes as an argument.
		{[]string{"-purpose", "promotional", "ada@example.com"}, nil, 2},
	} {
		var errOut bytes.Buffer
		got, code := parseConsentWithdrawPurposes(tc.args, &errOut)
		if code != tc.code || !slices.Equal(got, tc.want) {
			t.Errorf("%v: %v %d, want %v %d (%s)", tc.args, got, code, tc.want, tc.code, errOut.String())
		}
		if strings.Contains(errOut.String(), "ada@") {
			t.Errorf("%v: the usage names the address: %s", tc.args, errOut.String())
		}
	}
}

// The command reads one address, hands it to the withdrawal as it was typed
// (the service normalizes it), prints the record per purpose, and never
// the address; every failure has its exit code.
func TestConsentWithdrawCommand(t *testing.T) {
	now := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	both := []consent.Purpose{consent.PurposePromotional, consent.PurposeEventInvitations}
	for _, tc := range []struct {
		name, in string
		result   map[consent.Purpose]int64
		err      error
		code     int
		out      []string
		errOut   string
		called   bool
	}{
		{name: "done", in: "\n  Ada@Example.com \r\n\n", result: map[consent.Purpose]int64{consent.PurposePromotional: 2, consent.PurposeEventInvitations: 0},
			out: []string{"at: 2026-10-24T18:00:00Z", "promotional: 2 ended", "event_invitations: 0 ended"}, called: true},
		{name: "empty", in: " \n\n", code: 2, errOut: "no address"},
		{name: "two addresses", in: "ada@example.com\nbob@example.com\n", code: 2, errOut: "more than one line"},
		{name: "not an address", in: "ada\n", err: fmt.Errorf("%w: email", consent.ErrInvalid), code: 2, errOut: "not an e-mail address", called: true},
		{name: "off", in: "ada@example.com\n", err: consent.ErrDisabled, code: consentWithdrawExitOff, errOut: "are off", called: true},
		{name: "keycloak", in: "ada@example.com\n", err: fmt.Errorf("%w: identity: personal e-mail search failed with status 503", consent.ErrAddressesUnavailable),
			code: 1, errOut: "nothing ended, run it again", called: true},
		{name: "part", in: "ada@example.com\n", result: map[consent.Purpose]int64{consent.PurposePromotional: 1}, err: errors.New("database gone"),
			code: 1, out: []string{"promotional: 1 ended", "event_invitations: not done"}, errOut: "run it again", called: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			called := false
			code := consentWithdrawCommand(context.Background(), strings.NewReader(tc.in), &out, &errOut, now, both,
				func(_ context.Context, purposes []consent.Purpose, address string) (map[consent.Purpose]int64, error) {
					called = true
					if !slices.Equal(purposes, both) || address != strings.TrimSpace(strings.TrimSpace(tc.in)) {
						t.Errorf("withdraw(%v, %q)", purposes, address)
					}
					return tc.result, tc.err
				})
			if code != tc.code || called != tc.called {
				t.Fatalf("code %d called %v, want %d %v (out %q, err %q)", code, called, tc.code, tc.called, out.String(), errOut.String())
			}
			for _, line := range tc.out {
				if !strings.Contains(out.String(), line+"\n") {
					t.Errorf("out %q lacks %q", out.String(), line)
				}
			}
			if !strings.Contains(errOut.String(), tc.errOut) {
				t.Errorf("errOut %q lacks %q", errOut.String(), tc.errOut)
			}
			for _, written := range []string{out.String(), errOut.String()} {
				if strings.Contains(strings.ToLower(written), "example.com") {
					t.Errorf("the address was printed: %q", written)
				}
			}
		})
	}
}
