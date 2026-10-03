package authz

import (
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxClientLabels bounds the client label of the disagreement counter. The
// value is a token's azp, so only the realm's clients can appear; past this
// many the rest count as "other".
const maxClientLabels = 32

// disagreementLogEvery is how often one permission, side and client may log
// a line. The counter counts every check.
const disagreementLogEvery = time.Minute

// RoleMetrics counts, in the both mode, the Privileged checks where the
// Privileged Group and the role disagree, and serves them on /v1/metrics.
// Every label value is a role name, a side or a Keycloak client: nobody is
// named. Nil counts into nothing.
type RoleMetrics struct {
	logger *log.Logger
	now    func() time.Time

	mu      sync.Mutex
	mode    RoleMode
	counts  map[disagreement]uint64
	clients map[string]bool
	logged  map[disagreement]time.Time
}

type disagreement struct {
	permission string
	// grantedBy is "group" (the Privileged Group allows, the role does not:
	// the person loses this in the roles mode) or "role" (the role allows,
	// no Privileged Group does: the person gains it in the roles mode).
	grantedBy string
	client    string
}

// NewRoleMetrics logs one line per permission, side and client a minute to
// logger.
func NewRoleMetrics(logger *log.Logger, now func() time.Time) *RoleMetrics {
	if logger == nil {
		logger = log.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &RoleMetrics{
		logger: logger, now: now, mode: RoleModeGroups,
		counts: map[disagreement]uint64{}, clients: map[string]bool{}, logged: map[disagreement]time.Time{},
	}
}

func (m *RoleMetrics) setMode(mode RoleMode) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.mode = mode
	m.mu.Unlock()
}

// record counts one disagreement. group is the Privileged Group that
// allowed (ADMIN, YK or DK), empty when the role allowed.
func (m *RoleMetrics) record(permission, group, client string) {
	if m == nil {
		return
	}
	d := disagreement{permission: permission, grantedBy: "role"}
	if group != "" {
		d.grantedBy = "group"
	}
	now := m.now()
	m.mu.Lock()
	d.client = m.clientLabel(client)
	m.counts[d]++
	last, seen := m.logged[d]
	logLine := !seen || now.Sub(last) >= disagreementLogEvery
	if logLine {
		m.logged[d] = now
	}
	mode := m.mode
	m.mu.Unlock()
	if !logLine {
		return
	}
	line, _ := json.Marshal(struct {
		Event      string `json:"event"`
		Mode       string `json:"mode"`
		Permission string `json:"permission"`
		GrantedBy  string `json:"granted_by"`
		Group      string `json:"group,omitempty"`
		Client     string `json:"client"`
	}{"authz_role_disagreement", string(mode), d.permission, d.grantedBy, group, d.client})
	m.logger.Print(string(line))
}

// clientLabel is client as a label value; m.mu is held.
func (m *RoleMetrics) clientLabel(client string) string {
	if client == "" {
		return "none"
	}
	if m.clients[client] {
		return client
	}
	if len(m.clients) >= maxClientLabels {
		return "other"
	}
	m.clients[client] = true
	return client
}

// Prometheus renders the role mode as a gauge per mode and the
// disagreements per permission, side and client, in the text exposition
// format.
func (m *RoleMetrics) Prometheus() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	mode := m.mode
	keys := make([]disagreement, 0, len(m.counts))
	for d := range m.counts {
		keys = append(keys, d)
	}
	counts := make([]uint64, len(keys))
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.permission != b.permission {
			return a.permission < b.permission
		}
		if a.grantedBy != b.grantedBy {
			return a.grantedBy < b.grantedBy
		}
		return a.client < b.client
	})
	for i, d := range keys {
		counts[i] = m.counts[d]
	}
	m.mu.Unlock()

	var out strings.Builder
	out.WriteString("# TYPE skylab_authz_role_mode gauge\n")
	for _, each := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		value := "0"
		if each == mode {
			value = "1"
		}
		out.WriteString(`skylab_authz_role_mode{mode="` + string(each) + `"} ` + value + "\n")
	}
	out.WriteString("# TYPE skylab_authz_role_disagreements_total counter\n")
	for i, d := range keys {
		out.WriteString(`skylab_authz_role_disagreements_total{permission="` + d.permission + `",granted_by="` + d.grantedBy +
			`",client="` + labelValue(d.client) + `"} ` + strconv.FormatUint(counts[i], 10) + "\n")
	}
	return out.String()
}

// labelValue escapes a label value of the text exposition format.
func labelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
