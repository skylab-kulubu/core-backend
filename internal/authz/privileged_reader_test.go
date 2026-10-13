package authz

import (
	"slices"
	"strings"
	"testing"
)

// grup-yonetimi GY1 (ticket 08): DK reads everything a Privileged member
// reads, the Team audit record included, and changes nothing.

func TestPrivilegedReaderRootsFromEnv(t *testing.T) {
	t.Parallel()
	env := func(value string) func(string) string {
		return func(name string) string {
			if name != PrivilegedReaderRootsEnv {
				t.Fatalf("read %s", name)
			}
			return value
		}
	}
	roots := DefaultPrivilegedRoots()
	for _, tc := range []struct {
		raw   string
		roots []string
		want  []string
	}{
		{"", roots, []string{"/DK", "/UYELER/DK"}},
		{"  ", roots, []string{"/DK", "/UYELER/DK"}},
		// Unset keeps only the default reader roots that are roots.
		{"", []string{"/ADMIN", "/UYELER/DK"}, []string{"/UYELER/DK"}},
		{"", []string{"/ADMIN"}, []string{}},
		{"none", roots, []string{}},
		{" /UYELER/DK ", roots, []string{"/UYELER/DK"}},
		{"/DK,/UYELER/YK", roots, []string{"/DK", "/UYELER/YK"}},
	} {
		got, err := PrivilegedReaderRootsFromEnv(env(tc.raw), tc.roots)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%q over %v: %v, %v; want %v", tc.raw, tc.roots, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"/DK/ALT",         // under a root, not a root
		"/UYELER/ARGE",    // no root
		"/DK,,/UYELER/DK", // an empty entry
		"/DK,/DK",         // named twice
		"DK",              // no leading slash, no root
	} {
		got, err := PrivilegedReaderRootsFromEnv(env(raw), roots)
		if err == nil {
			t.Errorf("%q: %v, want an error", raw, got)
			continue
		}
		if !strings.Contains(err.Error(), PrivilegedReaderRootsEnv) {
			t.Errorf("%q: %v does not name %s", raw, err, PrivilegedReaderRootsEnv)
		}
	}
}

func TestDefaultPolicyReadsDKOnly(t *testing.T) {
	t.Parallel()
	if got := DefaultPolicy().PrivilegedReaderRoots; !slices.Equal(got, []string{"/DK", "/UYELER/DK"}) {
		t.Fatalf("reader roots = %v", got)
	}
}

// dkMember is a DK member as each mode sees them: the Group, and the roles
// Keycloak maps to DK (global:read, audit:read) from the both mode on.
func dkMember(mode RoleMode) Principal {
	p := Principal{ID: "dk", Roles: []string{RoleAuditRead}}
	if mode != RoleModeRoles {
		p.Groups = []string{"/UYELER/DK"}
	}
	if mode != RoleModeGroups {
		p.Roles = append(p.Roles, RoleGlobalRead)
	}
	return p
}

// ykMember is a YK member as each mode sees them: the Group, and every role
// of the contract from the both mode on.
func ykMember(mode RoleMode) Principal {
	p := Principal{ID: "yk", Roles: []string{RoleAuditRead}}
	if mode != RoleModeRoles {
		p.Groups = []string{"/UYELER/YK"}
	}
	if mode != RoleModeGroups {
		p.Roles = append(p.Roles, PermissionRoles()...)
	}
	return p
}

type decision struct {
	name string
	r    Resource
	a    Action
}

// readerReads are the reads of the panel a Privileged member makes:
// Events', Users' and Groups' lists and records, the Media library, every
// Team's applicants, certificates and templates, competitors and
// competitions, GitHub activity, private Media, short and form links, and
// the Team audit record.
var readerReads = []decision{
	{"group list/read", Resource{Type: TypeGroup}, Read},
	{"group list", Resource{Type: TypeGroup}, List},
	{"user read", Resource{Type: TypeUser}, Read},
	{"user full card", Resource{Type: TypeUser}, ReadFull},
	{"event read", Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Read},
	{"season read", Resource{Type: TypeSeason}, Read},
	{"applicants of any team", Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Read},
	{"applicants without a team", Resource{Type: TypeTicket}, Read},
	{"competitors", Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}, Read},
	{"competitions", Resource{Type: TypeCompetition, OwnerTeam: "WEBLAB"}, Read},
	{"media library", Resource{Type: TypeMedia}, List},
	{"private media", Resource{Type: TypeMediaReadLink}, Read},
	{"certificates", Resource{Type: TypeCertificate, OwnerTeam: "WEBLAB"}, Read},
	{"certificate templates", Resource{Type: TypeCertificateTemplate, OwnerTeam: "WEBLAB"}, Read},
	{"club default templates", Resource{Type: TypeCertificateTemplate}, Read},
	{"github activity", Resource{Type: TypeGithubActivity}, Read},
	{"audit record", Resource{Type: TypeTeamAudit}, Read},
}

// readerShortcutReads are reads through the Privileged Group shortcut alone
// (short links, form links): nothing in the roles mode, where url:moderator
// decides.
var readerShortcutReads = []decision{
	{"everyone's short links", Resource{Type: TypeURL}, Read},
	{"my short links", Resource{Type: TypeURL}, ReadMe},
	{"form links", Resource{Type: TypeFormLink}, Read},
}

// readerWrites are every change a Privileged member may make.
var readerWrites = []decision{
	{"group create", Resource{Type: TypeGroup}, Create},
	{"group update, members, role mappings", Resource{Type: TypeGroup}, Update},
	{"group delete", Resource{Type: TypeGroup}, Delete},
	{"user create", Resource{Type: TypeUser}, Create},
	{"user update, extra roles, logout", Resource{Type: TypeUser}, Update},
	{"user erasure", Resource{Type: TypeUser}, Delete},
	{"event create", Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Create},
	{"event create without a team", Resource{Type: TypeEvent}, Create},
	{"event update", Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Update},
	{"event delete", Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Delete},
	{"door staff", Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Assign},
	{"event day", Resource{Type: TypeEventDay, OwnerTeam: "WEBLAB"}, Update},
	{"session", Resource{Type: TypeSession, OwnerTeam: "WEBLAB"}, Create},
	{"season create", Resource{Type: TypeSeason}, Create},
	{"season update", Resource{Type: TypeSeason}, Update},
	{"season delete", Resource{Type: TypeSeason}, Delete},
	{"ticket assign", Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Assign},
	{"door check-in", Resource{Type: TypeTicket, OwnerTeam: "WEBLAB"}, Validate},
	{"competitor create", Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}, Create},
	{"competitor update", Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}, Update},
	{"competitor delete", Resource{Type: TypeCompetitor, OwnerTeam: "WEBLAB"}, Delete},
	{"competition create", Resource{Type: TypeCompetition, OwnerTeam: "WEBLAB"}, Create},
	{"competition update", Resource{Type: TypeCompetition, OwnerTeam: "WEBLAB"}, Update},
	{"competition delete", Resource{Type: TypeCompetition}, Delete},
	{"media delete", Resource{Type: TypeMedia}, Delete},
	{"event media upload", Resource{Type: TypeMedia, MediaUploader: MediaUploaderEventEditor}, Upload},
	{"certificate issue", Resource{Type: TypeCertificate, OwnerTeam: "WEBLAB"}, Issue},
	{"certificate revoke", Resource{Type: TypeCertificate, OwnerTeam: "WEBLAB"}, Revoke},
	{"template create", Resource{Type: TypeCertificateTemplate, OwnerTeam: "WEBLAB"}, Create},
	{"template update", Resource{Type: TypeCertificateTemplate}, Update},
	{"template binding", Resource{Type: TypeCertificateTemplate, OwnerTeam: "WEBLAB"}, Assign},
	{"short link create", Resource{Type: TypeURL}, Create},
	{"someone's short link update", Resource{Type: TypeURL, OwnerID: "other"}, Update},
	{"someone's short link delete", Resource{Type: TypeURL, OwnerID: "other"}, Delete},
	{"form link create", Resource{Type: TypeFormLink}, Create},
	{"form link update", Resource{Type: TypeFormLink}, Update},
}

func TestPrivilegedReader_ReadsEverythingChangesNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		az := NewAuthorizer(policyIn(mode))
		dk, yk := dkMember(mode), ykMember(mode)
		for _, d := range readerReads {
			if !az.Allow(dk, d.r, d.a) {
				t.Errorf("%s: DK %s = false", mode, d.name)
			}
			if !az.Allow(yk, d.r, d.a) {
				t.Errorf("%s: YK %s = false", mode, d.name)
			}
		}
		for _, d := range readerShortcutReads {
			if got := az.Allow(dk, d.r, d.a); got != (mode != RoleModeRoles) {
				t.Errorf("%s: DK %s = %v", mode, d.name, got)
			}
		}
		for _, d := range readerWrites {
			if az.Allow(dk, d.r, d.a) {
				t.Errorf("%s: DK %s = true", mode, d.name)
			}
			// YK's decisions do not move (except the short-link shortcut,
			// which is url:moderator's in the roles mode, as before).
			if d.r.Type == TypeURL || d.r.Type == TypeFormLink {
				continue
			}
			if !az.Allow(yk, d.r, d.a) {
				t.Errorf("%s: YK %s = false", mode, d.name)
			}
		}
	}
}

// A person in DK and YK writes through YK; a DK member who also leads a Team
// keeps that Team's Leader decisions.
func TestPrivilegedReader_OtherGroupsStillCount(t *testing.T) {
	t.Parallel()
	az := NewAuthorizer(DefaultPolicy())
	both := Principal{ID: "u", Groups: []string{"/UYELER/DK", "/UYELER/YK"}}
	if !az.Allow(both, Resource{Type: TypeGroup}, Update) || !az.Allow(both, Resource{Type: TypeEvent}, Create) {
		t.Fatal("DK and YK: YK's writes lost")
	}
	both = Principal{ID: "u", Groups: []string{"/UYELER/YK", "/DK/ALT"}}
	if !az.Allow(both, Resource{Type: TypeUser}, Update) {
		t.Fatal("YK and a DK subgroup: YK's writes lost")
	}
	leader := Principal{ID: "u", Groups: []string{"/UYELER/DK", "/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if !az.Allow(leader, Resource{Type: TypeEvent, OwnerTeam: "WEBLAB"}, Create) {
		t.Fatal("DK and a Leader: the Leader's Event create lost")
	}
	if az.Allow(leader, Resource{Type: TypeEvent, OwnerTeam: "ARTLAB"}, Create) {
		t.Fatal("DK and a Leader: another Team's Event created")
	}
	sub := Principal{ID: "u", Groups: []string{"/DK/BASKAN"}}
	if az.Allow(sub, Resource{Type: TypeGroup}, Update) || !az.Allow(sub, Resource{Type: TypeGroup}, Read) {
		t.Fatal("a DK subgroup is a reader too")
	}
}

// global:read is a new role: it allows nothing in the groups mode, nothing
// to a service account, and never a write; it is not audit:read.
func TestGlobalRead(t *testing.T) {
	t.Parallel()
	reader := Principal{ID: "u", Roles: []string{RoleGlobalRead}}
	if NewAuthorizer(policyIn(RoleModeGroups)).Allow(reader, Resource{Type: TypeGroup}, Read) {
		t.Error("groups mode: global:read alone reads Groups")
	}
	for _, mode := range []RoleMode{RoleModeBoth, RoleModeRoles} {
		az := NewAuthorizer(policyIn(mode))
		if !az.Allow(reader, Resource{Type: TypeUser}, ReadFull) {
			t.Errorf("%s: global:read does not read the full card", mode)
		}
		if az.Allow(reader, Resource{Type: TypeTeamAudit}, Read) {
			t.Errorf("%s: global:read reads the audit record", mode)
		}
		if az.Allow(reader, Resource{Type: TypeGroup}, Update) || az.Allow(reader, Resource{Type: TypeSeason}, Create) {
			t.Errorf("%s: global:read writes", mode)
		}
		service := reader
		service.ServiceAccount = true
		if az.Allow(service, Resource{Type: TypeGroup}, Read) {
			t.Errorf("%s: a service account reads with global:read", mode)
		}
	}
}

// AUTHZ_PRIVILEGED_READER_ROOTS=none: DK writes again (the way back).
func TestPrivilegedReader_None(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy()
	policy.PrivilegedReaderRoots = []string{}
	if !NewAuthorizer(policy).Allow(Principal{ID: "u", Groups: []string{"/UYELER/DK"}}, Resource{Type: TypeGroup}, Update) {
		t.Fatal("without reader roots DK does not write")
	}
}

func TestCapabilities_PrivilegedReader(t *testing.T) {
	t.Parallel()
	reads := []string{"group.read", "user.read", "user.readFull", "media.list", "media.readPrivate", "githubActivity.read", "audit.read"}
	shortcut := []string{"url.listMine", "url.moderate", "formLink.read"}
	teamReads := []string{"ticket.read", "competitor.read", "certificate.read", "certificateTemplate.read"}
	for _, mode := range []RoleMode{RoleModeGroups, RoleModeBoth, RoleModeRoles} {
		c := NewAuthorizer(policyIn(mode)).Capabilities(dkMember(mode))
		if !slices.Equal(c.Permissions, []string{RoleGlobalRead}) {
			t.Errorf("%s: permissions = %v", mode, c.Permissions)
		}
		want := slices.Clone(reads)
		if mode != RoleModeRoles {
			want = append(want, shortcut...)
		}
		slices.Sort(want)
		if got := trueKeys(c.Can); !slices.Equal(got, want) {
			t.Errorf("%s: can = %v, want %v", mode, got, want)
		}
		for _, scope := range []map[string]bool{c.OtherTeams.Can, c.NoOwnerTeam.Can} {
			want := slices.Clone(teamReads)
			slices.Sort(want)
			if got := trueKeys(scope); !slices.Equal(got, want) {
				t.Errorf("%s: team scope = %v, want %v", mode, got, want)
			}
		}
	}
}
