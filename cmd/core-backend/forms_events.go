package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// formsEventsEnv turns the core.forms consumer on with true, while
// EVENTS_ENABLED=true (docs/integration-events.md "core.forms"). Off (the
// default), core does not read core.forms and its events wait in the queue:
// the switch is how ops opens Forms' events after Forms publishes them,
// without a new image (Yusuf, 2026-11-24).
const formsEventsEnv = "EVENTS_FORMS_ENABLED"

// formsEventsEnabled reads formsEventsEnv: true, or false/unset. Any other
// value stops core.
func formsEventsEnabled(getenv func(string) string) (bool, error) {
	switch strings.TrimSpace(getenv(formsEventsEnv)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be true or false", formsEventsEnv)
	}
}

// formsEventPrincipal is the forms service's authority for the Ticket rule
// on its events. Only Forms' RabbitMQ user may publish on forms.events
// (ADR-0071 §4.1), as only Forms' service account may send the HTTP report:
// an event is the same report by another way, and both take the same rule
// (ticket.Service.RecordFormResponse). When the HTTP report and ticket:forms
// go (hub ticket R16), this stays the rule's only caller of that kind.
var formsEventPrincipal = authz.Principal{
	ID: "forms-events", Product: authz.ProductForms, Roles: []string{"ticket:forms"}, ServiceAccount: true, Client: "forms",
}

// formsRecordFunc is the Ticket rule with the forms service's authority.
func formsRecordFunc(tickets ticket.Service) formsfeed.RecordFunc {
	return func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
		return tickets.RecordFormResponse(ctx, formsEventPrincipal, r)
	}
}

// newFormsClient is the client of Forms' internal reads with core's own
// service account (KEYCLOAK_CLIENT_ID, client core, which carries
// skyforms:responses:read-service and aud ∋ forms), or nil without Forms'
// address or the account. It names the variable the address came from; an
// address that does not read is an error naming the variable.
func newFormsClient(getenv func(string) string, base, realm string) (*formsfeed.Client, string, error) {
	address, from, err := formsfeed.URLFromEnv(getenv)
	if err != nil || address == "" {
		return nil, from, err
	}
	clientID := strings.TrimSpace(getenv("KEYCLOAK_CLIENT_ID"))
	if base == "" || realm == "" || clientID == "" || strings.TrimSpace(getenv("KEYCLOAK_CLIENT_SECRET")) == "" {
		return nil, from, nil
	}
	httpClient := &http.Client{Timeout: skyMailTimeout}
	return &formsfeed.Client{
		BaseURL: address,
		HTTP:    httpClient,
		Tokens: &erasure.ClientCredentials{
			TokenURL: base + "/realms/" + realm + "/protocol/openid-connect/token",
			ClientID: clientID,
			HTTP:     httpClient,
			// Read for every token request, never kept (ADR-0050).
			Secret: func() (string, error) {
				if secret := strings.TrimSpace(getenv("KEYCLOAK_CLIENT_SECRET")); secret != "" {
					return secret, nil
				}
				return "", errors.New("KEYCLOAK_CLIENT_SECRET is not set")
			},
		},
	}, from, nil
}

// formsEventsRecorder is core.forms' recorder, or nil when the switch is
// off. On, it needs Forms' client (a guest's Ticket is written from the
// single answer read): without it core stops, naming what is missing.
func formsEventsRecorder(getenv func(string) string, client *formsfeed.Client, rec *formsfeed.Recorder) *formsfeed.Recorder {
	on, err := formsEventsEnabled(getenv)
	if err != nil {
		log.Fatal(err)
	}
	if !on {
		return nil
	}
	if client == nil {
		log.Fatalf("%s=true needs Forms' internal address (%s or ACCOUNT_ERASURE_FORMS_URL), KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET", formsEventsEnv, formsfeed.URLEnv)
	}
	return rec
}

// formsClientFromEnv is newFormsClient for startup: an address that does
// not read warns and leaves Forms' reads off (the pulls, and core.forms,
// which then stops core if it is on), as the tracking withdrawals do.
func formsClientFromEnv(getenv func(string) string, base, realm string) *formsfeed.Client {
	client, from, err := newFormsClient(getenv, base, realm)
	if err != nil {
		// The error names the variable, never its value.
		log.Printf("WARNING forms reads: %v; pulls from Forms off", err)
		return nil
	}
	if client == nil {
		log.Printf("forms reads: off (no Forms internal address or no core service account); backfill answers 503 forms_pull_off")
		return nil
	}
	log.Printf("forms reads: on, Forms' address from %s; daily catch-up and backfill pulls run", from)
	return client
}

// formsMetrics is the answers' counters (events and pulls share one
// recorder) and the pulls'.
type formsMetrics struct {
	recorder *formsfeed.Recorder
	puller   *formsfeed.Puller
}

func (m formsMetrics) Prometheus() string {
	out := ""
	if m.recorder != nil {
		out += m.recorder.Prometheus()
	}
	if m.puller != nil {
		out += m.puller.Prometheus()
	}
	return out
}

// joinedMetrics prints each in turn.
type joinedMetrics []interface{ Prometheus() string }

func (j joinedMetrics) Prometheus() string {
	out := ""
	for _, m := range j {
		out += m.Prometheus()
	}
	return out
}
