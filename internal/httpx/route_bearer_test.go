package httpx_test

import (
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/mapregion"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// routesAheadOfBearer are the routes served before Bearer on purpose: they
// take no sign-in, or carry a permission of their own (a key or signed
// token in the path or link, the self-delete bearer, Guest apply's
// optional token: an operator's, or none on the internal network, while
// the edge answers 404 without a token and 401 to a token it cannot verify,
// guest_apply_test.go).
// Every other route reads the token through Bearer. A
// route that needs the caller does not belong here: registered ahead of
// Bearer it answers 401 to every token (/v1/map-regions did).
var routesAheadOfBearer = []string{
	"GET /v1/health",
	"GET /v1/metrics",
	"GET /v1/ready",
	"GET /v1/go/:alias/qr",
	"GET /v1/go/c/:serial",
	"GET /c/:serial",
	"GET /v1/public/certificates/:serial",
	"GET /v1/certificates/verify/:serial/pdf",
	"GET /v1/certificates/verify/:serial/qr",
	"GET /v1/certificates/verify/:serial",
	"GET /v1/public/tickets/:key",
	"GET /v1/public/tickets/:key/qr.png",
	"GET /v1/public/tickets/:key/pdf",
	"GET /v1/public/tickets/:key/check-ins",
	"POST /v1/public/tickets/:key/cancel",
	"GET /v1/skypass/jwks",
	"GET /v1/door/jwks",
	"POST /v1/account-deletion-requests/self",
	"GET /v1/account-deletion-requests/status",
	"POST /v1/account-deletion-requests/status/retry",
	"GET " + consent.WithdrawPath,
	"POST " + consent.WithdrawPath,
	"GET " + consent.ConfirmPath,
	"POST " + consent.ConfirmPath,
	"GET " + commpref.UnsubscribePath,
	"POST " + commpref.UnsubscribePath,
	"POST /v1/events/:eventId/applications/guest",
	"GET /v1/media/:id/content",
}

// Routes the assembled app below must serve, so the check reaches the
// route groups their dependencies turn on.
var routesBehindBearerSample = []string{
	"GET /v1/map-regions",
	"POST /v1/map-regions/:id/retire",
	"GET /v1/dashboard/github-activity",
	"GET /v1/dashboard/summary",
	"GET /v1/dashboard/event-stats",
	"POST /v1/events/:id/feedback/send",
	"POST /v1/events/:id/forms/:formId/backfill",
	"GET /v1/events/:id/forms/:formId/questions",
	"GET /v1/events/:id/forms/:formId/mapping",
	"PUT /v1/events/:id/forms/:formId/mapping",
	"DELETE /v1/events/:id/forms/:formId/mapping",
	"GET /v1/events/:id/respondents",
	"POST /v1/events/:id/respondents/tickets/preview",
	"POST /v1/events/:id/respondents/tickets",
	"GET /v1/events/:id/respondents/ticket-jobs/:jobId",
	"GET /v1/events/:id/certificate-rule",
	"PUT /v1/events/:id/certificate-rule",
	"GET /v1/events/:id/organizers",
	"PUT /v1/events/:id/organizers",
	"GET /v1/events/:eventId/certificates/attendance",
	"POST /v1/events/:eventId/certificates/rescan",
	"GET /v1/events/:eventId/certificate-template",
	"POST /v1/events/:eventId/certificate-template",
	"POST /v1/events/:eventId/certificates/mail/resend",
	"POST /v1/certificates/:serial/mail/resend",
	"POST /v1/events/:eventId/certificates/issue",
	"POST /v1/events/:eventId/mail-list",
	"GET /v1/apps",
	"GET " + handlers.AppPreferencePath,
	"POST /v1/skypass/qr",
	"GET /v1/events/:eventId/competitions",
	"GET /v1/subject-requests",
	"POST /v1/tickets/search",
	"GET /v1/users/me/capabilities",
	"GET /v1/users/directory",
	"GET /v1/certificates/me",
	"POST /v1/tickets/:id/notification",
	"POST /v1/tickets/:id/cancel",
	"POST /v1/tickets/:id/restore",
	"POST /v1/tickets/:id/promote",
	"POST /v1/events/:eventId/attendee-imports/preview",
	"POST /v1/events/:eventId/attendee-imports",
	"POST /v1/events/:eventId/exports",
	"DELETE /v1/tickets/:ticketId/sessions/:sessionId/check-in",
	"DELETE /v1/sessions/:sessionId/check-ins/:checkInId",
	"POST /v1/events/:eventId/entries/scan",
	"POST /v1/events/:eventId/entries/resolve",
	"GET /v1/events/:eventId/entries/summary",
	"GET /v1/events/:eventId/door-staff/stats",
	"GET /v1/events/:eventId/entries",
	"DELETE /v1/events/:eventId/entries/:entryId",
	"POST /v1/sessions/:sessionId/check-in/me",
	"POST /v1/sessions/:sessionId/check-in/guest",
	"POST /v1/sessions/:sessionId/door-qr",
	"GET /v1/events/places",
	"GET /v1/event-themes",
	"GET /v1/users/:id/account",
	"POST /v1/users/:id/password",
	"DELETE /v1/users/:id/credentials/:credentialId",
}

// Never called: Bearer refuses the probe token before any handler runs.
type (
	unusedSelfDeletion struct {
		handlers.AccountDeletionService
	}
	unusedGithubActivity   struct{ handlers.GithubActivitySource }
	unusedFeedbackCounter  struct{ handlers.FeedbackCounter }
	unusedFeedbackSender   struct{ handlers.FeedbackSender }
	unusedFeedbackReminder struct{ handlers.FeedbackReminders }
	unusedEventMail        struct{ eventmail.Service }
	unusedFormBackfills    struct{ handlers.FormBackfills }
	unusedFormMappings     struct{ handlers.FormMappings }
	unusedFormRespondents  struct{ handlers.FormRespondents }
)

var segmentParam = regexp.MustCompile(`/:[A-Za-z0-9_]+`)

// Every route but those listed ahead of Bearer reads the token through
// Bearer: a token sent to it is verified, and a bad one answers 401 before
// the handler. Read from the assembled app's route table, so a route added
// later is checked by itself. (The management membership routes, served
// only in apply mode, are not assembled here.)
func TestEveryRouteButThePublicOnesReadsTheBearer(t *testing.T) {
	var parsed atomic.Int32
	deps := memoryDeps()
	deps.ParseToken = func(string) (authn.Identity, error) {
		parsed.Add(1)
		return authn.Identity{}, authn.ErrInvalidToken
	}
	deps.MapRegions = &mapregion.Service{}
	deps.TicketPage = ticketpage.New(ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), nil, nil, nil, ticketpage.Config{})
	deps.SelfDeletion = unusedSelfDeletion{}
	deps.GithubActivity = unusedGithubActivity{}
	deps.FeedbackInvites = unusedFeedbackCounter{}
	deps.FeedbackSender = unusedFeedbackSender{}
	deps.FeedbackReminders = unusedFeedbackReminder{}
	deps.EventMail = unusedEventMail{}
	deps.FormBackfills = unusedFormBackfills{}
	deps.FormMappings = unusedFormMappings{}
	deps.FormRespondents = unusedFormRespondents{}
	deps.AppAssignments = true
	clients, err := apppref.ParseClients("forms:skyforms")
	if err != nil {
		t.Fatal(err)
	}
	deps.AppPreferences = apppref.NewService(clients, apppref.NewMemoryStore())
	app := httpx.New(deps)

	served := map[string]bool{}
	for _, route := range app.GetRoutes(true) {
		if route.Method != fiber.MethodHead {
			served[route.Method+" "+route.Path] = true
		}
	}
	public := map[string]bool{}
	for _, route := range routesAheadOfBearer {
		public[route] = true
		if !served[route] {
			t.Errorf("%s is listed ahead of Bearer but not served", route)
		}
	}
	for _, route := range routesBehindBearerSample {
		if !served[route] {
			t.Errorf("%s is not served: the check misses its route group", route)
		}
	}
	routes := make([]string, 0, len(served))
	for route := range served {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	checked := 0
	for _, route := range routes {
		if public[route] {
			continue
		}
		method, pattern, _ := strings.Cut(route, " ")
		path := segmentParam.ReplaceAllString(pattern, "/"+uuid.NewString())
		path = strings.ReplaceAll(path, `\:`, ":")
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer probe")
		before := parsed.Load()
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		resp.Body.Close()
		if parsed.Load() == before || resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("%s (%s): %d without reading the token; register it after Bearer, or list it ahead of Bearer if it takes no sign-in", route, path, resp.StatusCode)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("only %d routes checked", checked)
	}
}
