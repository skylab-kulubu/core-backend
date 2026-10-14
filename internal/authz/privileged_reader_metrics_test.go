package authz

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// In the both mode a DK member's reads are no disagreement: the Group
// allows them, and in the roles mode global:read does. Keycloak gives DK
// global:read and audit:read and takes url:moderator off (url:access stays:
// one's own links, Yusuf 2026-10-14); counting the short-link and form-link
// reads as url:moderator disagreements would cloud the both → roles gate
// (grup-yonetimi 09). Measurement only: the decisions do not move.
func TestBothMode_ReaderReadsAreNoDisagreement(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	metrics := NewRoleMetrics(log.New(&logs, "", 0), time.Now)
	az := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(metrics))

	dk := dkMember(RoleModeBoth)
	dk.Client = "admin"
	dk.Roles = append(dk.Roles, RoleURLAccess)
	for _, list := range [][]decision{readerReads, readerShortcutReads} {
		for _, d := range list {
			if !az.Allow(dk, d.r, d.a) {
				t.Errorf("DK %s = false", d.name)
			}
		}
	}
	if text := metrics.Prometheus(); strings.Contains(text, "skylab_authz_role_disagreements_total{") {
		t.Errorf("DK's reads counted as disagreements:\n%s", text)
	}
	if logs.Len() != 0 {
		t.Errorf("DK's reads logged disagreements:\n%s", logs.String())
	}
}

// global:read stands in only for reads, and only for whoever holds it: a DK
// member without it (Keycloak not yet released) and a YK member without
// url:moderator still count, as before.
func TestBothMode_ReaderDisagreementsStillCounted(t *testing.T) {
	t.Parallel()
	metrics := NewRoleMetrics(log.New(&bytes.Buffer{}, "", 0), time.Now)
	az := NewAuthorizer(policyIn(RoleModeBoth), WithRoleMetrics(metrics))

	noGlobalRead := Principal{ID: "dk", Client: "admin", Groups: []string{"/UYELER/DK"}, Roles: []string{RoleURLAccess}}
	if !az.Allow(noGlobalRead, Resource{Type: TypeURL}, Read) {
		t.Fatal("DK without global:read refused everyone's short links")
	}
	yk := Principal{ID: "yk", Client: "skyl", Groups: []string{"/UYELER/YK"}, Roles: []string{RoleURLAccess}}
	if !az.Allow(yk, Resource{Type: TypeURL}, Read) {
		t.Fatal("YK refused everyone's short links")
	}
	text := metrics.Prometheus()
	for _, want := range []string{
		`skylab_authz_role_disagreements_total{permission="url:moderator",granted_by="group",client="admin"} 1`,
		`skylab_authz_role_disagreements_total{permission="url:moderator",granted_by="group",client="skyl"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q:\n%s", want, text)
		}
	}
}
