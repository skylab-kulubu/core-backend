package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// subjectRequestReceived is ten days ago in Turkey: past, and within the
// year a request's day may lie back.
var subjectRequestReceived = time.Now().In(time.FixedZone("TRT", 3*60*60)).AddDate(0, 0, -10)

// subjectRequestBody is a request received ten days ago.
var subjectRequestBody = `{"receivedOn":"` + subjectRequestReceived.Format(time.DateOnly) + `","channel":"email",` +
	`"requesterName":"Ada Lovelace","requesterContact":"ada@example.test","requestTypes":["erase"]}`

// The register's routes: YK records, reads, closes; the answer is never
// cached; a refused field names its code; nobody else gets in.
func TestSubjectRequestRoutes(t *testing.T) {
	t.Parallel()
	e := newRoleModeEnv(t, authz.RoleModeGroups)
	yk := e.token(t, []string{"/UYELER/YK"})

	created := sendJSON(t, e.app, yk, fiber.MethodPost, "/v1/subject-requests", subjectRequestBody)
	if created.status != fiber.StatusCreated || created.body["dueOn"] != subjectRequestReceived.AddDate(0, 0, 30).Format(time.DateOnly) || created.body["status"] != "received" ||
		created.body["responder"] != "YK Başkanı" {
		t.Fatalf("create %d %v", created.status, created.body)
	}
	id := created.body["id"].(string)

	answered := subjectRequestReceived.AddDate(0, 0, 5).Format(time.DateOnly)
	stale := sendJSON(t, e.app, yk, fiber.MethodPatch, "/v1/subject-requests/"+id,
		`{"responder":"","updatedAt":"2001-01-01T00:00:00Z"}`)
	if stale.status != fiber.StatusConflict || stale.body["code"] != "subject_request_changed" {
		t.Fatalf("stale version %d %v", stale.status, stale.body)
	}
	closed := sendJSON(t, e.app, yk, fiber.MethodPatch, "/v1/subject-requests/"+id,
		`{"status":"answered","answeredOn":"`+answered+`","answerSummary":"Silindi.","updatedAt":"`+created.body["updatedAt"].(string)+`"}`)
	if closed.status != fiber.StatusOK || closed.body["answeredOn"] != answered {
		t.Fatalf("close %d %v", closed.status, closed.body)
	}
	read := sendJSON(t, e.app, yk, fiber.MethodGet, "/v1/subject-requests/"+id, "")
	if events, _ := read.body["events"].([]any); read.status != fiber.StatusOK || len(events) != 2 {
		t.Fatalf("read %d %v", read.status, read.body)
	}

	req := httptest.NewRequest(fiber.MethodGet, "/v1/subject-requests", nil)
	req.Header.Set("Authorization", "Bearer "+yk)
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil || resp.StatusCode != fiber.StatusOK || len(list) != 1 ||
		resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("list %d %s %q", resp.StatusCode, raw, resp.Header.Get(fiber.HeaderCacheControl))
	}

	refused := sendJSON(t, e.app, yk, fiber.MethodPost, "/v1/subject-requests", strings.Replace(subjectRequestBody, `"email"`, `"fax"`, 1))
	if refused.status != fiber.StatusBadRequest || refused.body["code"] != "channel_invalid" {
		t.Fatalf("refused %d %v", refused.status, refused.body)
	}
	if got := sendJSON(t, e.app, yk, fiber.MethodPatch, "/v1/subject-requests/"+id, `{"status":"rejected","answeredOn":null}`); got.status != fiber.StatusBadRequest ||
		got.body["code"] != "answered_on_required" {
		t.Fatalf("closing without a date %d %v", got.status, got.body)
	}
	if got := sendJSON(t, e.app, yk, fiber.MethodPost, "/v1/subject-requests", strings.Replace(subjectRequestBody, `"channel"`, `"userId":"ada","channel"`, 1)); got.status != fiber.StatusBadRequest ||
		got.body["code"] != "user_invalid" {
		t.Fatalf("account id %d %v", got.status, got.body)
	}
	if got := sendJSON(t, e.app, yk, fiber.MethodGet, "/v1/subject-requests/nope", ""); got.status != fiber.StatusNotFound {
		t.Fatalf("bad id %d", got.status)
	}

	for name, token := range map[string]string{
		"DK":              e.token(t, []string{"/UYELER/DK"}, authz.RoleGlobalRead),
		"member":          e.token(t, []string{"/UYELER/ARGE/WEBLAB"}),
		"service account": e.serviceToken(t, authz.RoleSubjectRequestManage),
	} {
		for _, path := range []string{"/v1/subject-requests", "/v1/subject-requests/" + id} {
			if got := e.status(t, token, fiber.MethodGet, path, ""); got != fiber.StatusForbidden {
				t.Errorf("%s GET %s: %d", name, path, got)
			}
		}
	}
	anonymous, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/subject-requests", nil))
	if err != nil {
		t.Fatal(err)
	}
	if anonymous.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous %d", anonymous.StatusCode)
	}
}
