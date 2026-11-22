package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type accountsFixture struct {
	people   *identity.Memory
	accounts *identity.MemoryAccounts
	users    *user.MemoryStore
	audit    *teamaudit.Memory
	svc      *identity.UserAccounts
	member   uuid.UUID
	admin    uuid.UUID
	yk       uuid.UUID
	dk       uuid.UUID
	now      time.Time
}

func newAccountsFixture(t *testing.T) *accountsFixture {
	t.Helper()
	f := &accountsFixture{
		people: identity.NewMemory(), accounts: identity.NewMemoryAccounts(), users: user.NewMemoryStore(),
		audit: teamaudit.NewMemory(), member: uuid.New(), admin: uuid.New(), yk: uuid.New(), dk: uuid.New(),
		now: time.Date(2026, 11, 22, 21, 0, 0, 0, time.UTC),
	}
	for _, g := range []identity.Group{
		{ID: uuid.NewString(), Name: "ADMIN", Path: "/ADMIN"},
		{ID: uuid.NewString(), Name: "YK", Path: "/UYELER/YK"},
		{ID: uuid.NewString(), Name: "DK", Path: "/UYELER/DK"},
		{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
	} {
		f.people.PutGroup(g)
	}
	for id, path := range map[uuid.UUID]string{f.member: "/UYELER/ARGE/WEBLAB", f.admin: "/ADMIN", f.yk: "/UYELER/YK", f.dk: "/UYELER/DK"} {
		f.people.PutUser(identity.Person{ID: id, Email: id.String()[:8] + "@example.com", FirstName: "Ada", LastName: "Lovelace", Username: id.String()[:8]})
		group, err := f.people.GetGroup(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.people.AddMember(context.Background(), group.ID, id); err != nil {
			t.Fatal(err)
		}
		f.accounts.PutAccount(identity.AccountUser{
			ID: id, Username: id.String()[:8], Email: "ada@example.com", EmailVerified: true, Enabled: true,
			FirstName: "Ada", LastName: "Lovelace",
			Attributes: map[string][]string{
				"schoolEmail":             {"ada@std.yildiz.edu.tr"},
				"personalEmail":           {"ada@example.com"},
				"personalEmailVerifiedAt": {"2026-09-21T14:13:20Z"},
			},
		})
	}
	f.svc = identity.NewUserAccounts(identity.UserAccountOptions{
		Accounts: f.accounts, People: f.people, Users: f.users,
		Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Audit: f.audit,
		Now: func() time.Time { return f.now },
	})
	return f
}

func asAdmin(f *accountsFixture) authz.Principal {
	return authz.Principal{ID: f.admin.String(), Groups: []string{"/ADMIN"}, Client: "admin"}
}

func asYK(f *accountsFixture) authz.Principal {
	return authz.Principal{ID: f.yk.String(), Groups: []string{"/UYELER/YK"}, Client: "admin"}
}

func asDK(f *accountsFixture) authz.Principal {
	return authz.Principal{ID: f.dk.String(), Groups: []string{"/UYELER/DK"}, Client: "admin"}
}

// actorContext is the request's context as the handlers make it: the
// caller in the audit actor.
func accountActor(p authz.Principal) context.Context {
	id, _ := uuid.Parse(p.ID)
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged, Client: p.Client, CorrelationID: "req-1"})
}

func accountCode(err error) string {
	var refused *identity.AccountError
	if errors.As(err, &refused) {
		return refused.Code
	}
	return ""
}

func TestAccountReadsEverythingAndNoSecret(t *testing.T) {
	f := newAccountsFixture(t)
	start := f.now.Add(-time.Hour)
	f.accounts.PutCredentials(f.member,
		identity.AccountCredential{ID: uuid.NewString(), Type: "password", CreatedAt: &start},
		identity.AccountCredential{ID: uuid.NewString(), Type: "webauthn-passwordless", Label: "MacBook"})
	f.accounts.PutLinks(f.member, identity.LinkedIdentity{Provider: "OBS", UserName: "ada@std.yildiz.edu.tr"})
	f.accounts.PutSessions(f.member, identity.AccountSession{Start: start, LastAccess: f.now, Clients: []string{"account-center"}})
	f.accounts.PutLockout(f.member, identity.AccountLockout{Locked: true, Failures: 10})
	f.accounts.PutEvents(f.member,
		identity.LoginEvent{Type: "LOGIN_ERROR", Time: f.now, Client: "admin", Error: "invalid_user_credentials"},
		identity.LoginEvent{Type: "LOGIN", Time: start, Client: "admin"})

	for _, p := range []authz.Principal{asAdmin(f), asYK(f), asDK(f)} {
		account, err := f.svc.Account(context.Background(), p, f.member)
		if err != nil {
			t.Fatalf("%v: %v", p.Groups, err)
		}
		if account.Status != user.ReadStatusActive || !account.Enabled || !account.YTUVerified {
			t.Fatalf("account = %+v", account)
		}
		if account.Email.PrimaryKind != identity.PrimaryPersonal || !account.Email.PersonalVerified || account.Email.School != "ada@std.yildiz.edu.tr" {
			t.Fatalf("email = %+v", account.Email)
		}
		if len(account.Credentials) != 2 || len(account.Sessions) != 1 || !account.Lockout.Locked {
			t.Fatalf("account = %+v", account)
		}
		if account.Logins == nil || account.Logins.Last == nil || !account.Logins.Last.Equal(start) {
			t.Fatalf("logins = %+v", account.Logins)
		}
		// DK reads, and changes nothing.
		if wantManage := p.Groups[0] != "/UYELER/DK"; account.Can.Manage != wantManage {
			t.Fatalf("%v: can = %+v", p.Groups, account.Can)
		}
	}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if _, err := f.svc.Account(context.Background(), member, f.member); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("a member read: %v", err)
	}

	f.accounts.NoEvents = true
	account, err := f.svc.Account(context.Background(), asAdmin(f), f.member)
	if err != nil || account.Logins != nil {
		t.Fatalf("without view-events: %+v %v", account.Logins, err)
	}
	raw, _ := json.Marshal(account)
	if !strings.Contains(string(raw), `"logins":null`) {
		t.Fatalf("logins not null: %s", raw)
	}
}

func TestAccountOfAPrivilegedPersonIsADMINsAlone(t *testing.T) {
	f := newAccountsFixture(t)
	// YK may manage a member, not ADMIN, YK or DK.
	if account, _ := f.svc.Account(context.Background(), asYK(f), f.admin); account.Can.Manage || account.Can.Reason != identity.AccountPrivilegedTarget {
		t.Fatalf("YK on ADMIN: %+v", account.Can)
	}
	err := f.svc.SetEnabled(accountActor(asYK(f)), asYK(f), f.dk, false)
	if accountCode(err) != identity.AccountPrivilegedTarget {
		t.Fatalf("YK disables DK: %v", err)
	}
	if u, _ := f.accounts.Get(f.dk); !u.Enabled {
		t.Fatal("DK was disabled")
	}
	if err := f.svc.SetEnabled(accountActor(asAdmin(f)), asAdmin(f), f.dk, false); err != nil {
		t.Fatalf("ADMIN disables DK: %v", err)
	}
	if err := f.svc.SetEnabled(accountActor(asYK(f)), asYK(f), f.member, false); err != nil {
		t.Fatalf("YK disables a member: %v", err)
	}
	// DK writes nothing, whatever the target.
	if err := f.svc.SetEnabled(accountActor(asDK(f)), asDK(f), f.member, true); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("DK enables: %v", err)
	}
	rows := f.audit.Rows()
	var denied []string
	for _, row := range rows {
		if row.Outcome == teamaudit.OutcomeDenied {
			denied = append(denied, row.ErrorCode)
			if row.Actor.Basis != teamaudit.BasisNone || row.TargetUserID == uuid.Nil || row.GroupID != "" {
				t.Fatalf("denied row = %+v", row)
			}
		}
	}
	if !slices.Equal(denied, []string{identity.AccountPrivilegedTarget, "forbidden"}) {
		t.Fatalf("denied rows = %v", denied)
	}
}

func TestOwnAccountIsChangedInTheAccountCenter(t *testing.T) {
	f := newAccountsFixture(t)
	p := asAdmin(f)
	if account, _ := f.svc.Account(context.Background(), p, f.admin); account.Can.Manage || account.Can.Reason != identity.AccountOwn {
		t.Fatalf("own card: %+v", account.Can)
	}
	for name, err := range map[string]error{
		"disable":  f.svc.SetEnabled(accountActor(p), p, f.admin, false),
		"password": func() error { _, err := f.svc.SetTemporaryPassword(accountActor(p), p, f.admin, ""); return err }(),
		"email":    f.svc.ChangeEmail(accountActor(p), p, f.admin, identity.EmailChange{Address: "new@example.com"}),
		"lockout":  f.svc.ClearLockout(accountActor(p), p, f.admin),
	} {
		if accountCode(err) != identity.AccountOwn {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if u, _ := f.accounts.Get(f.admin); !u.Enabled || f.accounts.Passwords[f.admin] != 0 {
		t.Fatalf("own account changed: %+v", u)
	}
}

func TestDisableEndsSessionsAndIsRecorded(t *testing.T) {
	f := newAccountsFixture(t)
	f.accounts.PutSessions(f.member, identity.AccountSession{Start: f.now})
	p := asYK(f)
	if err := f.svc.SetEnabled(accountActor(p), p, f.member, false); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.accounts.Get(f.member); u.Enabled || f.accounts.Logouts[f.member] != 1 {
		t.Fatalf("disabled = %+v, logouts %d", u, f.accounts.Logouts[f.member])
	}
	rows := f.audit.Rows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionUserDisable || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].TargetUserID != f.member || !*rows[0].Before.Enabled || *rows[0].After.Enabled {
		t.Fatalf("rows = %+v", rows)
	}
	// Again: no change, no row.
	if err := f.svc.SetEnabled(accountActor(p), p, f.member, false); err != nil || len(f.audit.Rows()) != 1 {
		t.Fatalf("again: %v, %d rows", err, len(f.audit.Rows()))
	}
	if err := f.svc.SetEnabled(accountActor(p), p, f.member, true); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.accounts.Get(f.member); !u.Enabled {
		t.Fatal("not enabled again")
	}
}

func TestNoAuditRowNoChange(t *testing.T) {
	f := newAccountsFixture(t)
	f.audit.SetUnavailable(true)
	p := asYK(f)
	err := f.svc.SetEnabled(accountActor(p), p, f.member, false)
	if !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if u, _ := f.accounts.Get(f.member); !u.Enabled {
		t.Fatal("disabled without a row")
	}
	// Without an actor in the context there is no row either.
	if _, err := f.svc.SetTemporaryPassword(context.Background(), p, f.member, ""); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("no actor: %v", err)
	}
}

var generatedPassword = regexp.MustCompile(`^[a-km-zA-HJ-NP-Z2-9]{4}(-[a-km-zA-HJ-NP-Z2-9]{4}){3}$`)

func TestTemporaryPasswordIsShownOnceAndRecordedNowhere(t *testing.T) {
	f := newAccountsFixture(t)
	f.accounts.PutSessions(f.member, identity.AccountSession{Start: f.now})
	var logged []string
	f.svc = identity.NewUserAccounts(identity.UserAccountOptions{
		Accounts: f.accounts, People: f.people, Users: f.users,
		Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Audit: f.audit,
		Logf: func(format string, args ...any) { logged = append(logged, format) },
	})
	p := asYK(f)
	result, err := f.svc.SetTemporaryPassword(accountActor(p), p, f.member, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Generated || !result.Temporary || !result.SessionsClosed || !generatedPassword.MatchString(result.Password) {
		t.Fatalf("result = %+v", result)
	}
	if u, _ := f.accounts.Get(f.member); !slices.Contains(u.RequiredActions, "UPDATE_PASSWORD") || f.accounts.Logouts[f.member] != 1 {
		t.Fatalf("account = %+v", u)
	}
	typed, err := f.svc.SetTemporaryPassword(accountActor(p), p, f.member, "Yeni-parola-2026")
	if err != nil || typed.Generated || typed.Password != "" {
		t.Fatalf("typed = %+v %v", typed, err)
	}
	for _, row := range f.audit.Rows() {
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), result.Password) || strings.Contains(string(raw), "Yeni-parola-2026") {
			t.Fatalf("a row holds the password: %s", raw)
		}
		if row.Action != teamaudit.ActionUserPasswordSet || !*row.After.Temporary {
			t.Fatalf("row = %+v", row)
		}
	}
	if *f.audit.Rows()[0].After.Generated != true || *f.audit.Rows()[1].After.Generated != false {
		t.Fatalf("generated flags: %+v", f.audit.Rows())
	}
	if len(logged) != 0 {
		t.Fatalf("logged: %v", logged)
	}

	f.accounts.PasswordPolicy = 30
	_, err = f.svc.SetTemporaryPassword(accountActor(p), p, f.member, "kisa-parola")
	var refused *identity.AccountError
	if !errors.As(err, &refused) || refused.Code != identity.AccountPasswordPolicy || refused.Policy != "invalidPasswordMinLengthMessage" {
		t.Fatalf("policy: %v", err)
	}
	if strings.Contains(err.Error(), "kisa-parola") {
		t.Fatal("the error names the password")
	}
	last := f.audit.Rows()[len(f.audit.Rows())-1]
	if last.Outcome != teamaudit.OutcomeFailed || last.ErrorCode != identity.AccountPasswordPolicy {
		t.Fatalf("refused row = %+v", last)
	}
}

func TestGeneratedPasswordsDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		password, err := identity.GeneratePassword()
		if err != nil || !generatedPassword.MatchString(password) || seen[password] {
			t.Fatalf("%q %v", password, err)
		}
		seen[password] = true
	}
}

func TestMailsGoToThePrimaryEmail(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	if err := f.svc.SendPasswordMail(accountActor(p), p, f.member); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SendVerificationMail(accountActor(p), p, f.member); err != nil {
		t.Fatal(err)
	}
	if got := f.accounts.Mails[f.member]; !slices.Equal(got, []string{"actions:UPDATE_PASSWORD", "verify"}) {
		t.Fatalf("mails = %v", got)
	}
	f.accounts.FailMail = true
	if err := f.svc.SendPasswordMail(accountActor(p), p, f.member); accountCode(err) != identity.AccountMailFailed {
		t.Fatalf("failed mail: %v", err)
	}
	f.accounts.FailMail = false
	u, _ := f.accounts.Get(f.member)
	u.Email = ""
	f.accounts.PutAccount(u)
	if err := f.svc.SendVerificationMail(accountActor(p), p, f.member); accountCode(err) != identity.AccountNoEmail {
		t.Fatalf("no address: %v", err)
	}
}

func TestChangeEmail(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	ctx := accountActor(p)
	for _, c := range []struct {
		address, code string
	}{
		{"not-an-address", identity.AccountEmailInvalid},
		{"ada@std.yildiz.edu.tr", identity.AccountSchoolDomain},
		{"ada@ce.yildiz.edu.tr", identity.AccountSchoolDomain},
	} {
		if err := f.svc.ChangeEmail(ctx, p, f.member, identity.EmailChange{Address: c.address}); accountCode(err) != c.code {
			t.Fatalf("%s: %v", c.address, err)
		}
	}
	// Another account holds it (as its Personal e-mail).
	other, _ := f.accounts.Get(f.admin)
	other.Attributes["personalEmail"] = []string{"taken@example.com"}
	other.Email = "taken@example.com"
	f.accounts.PutAccount(other)
	if err := f.svc.ChangeEmail(ctx, p, f.member, identity.EmailChange{Address: " Taken@Example.com "}); accountCode(err) != identity.AccountEmailTaken {
		t.Fatalf("taken: %v", err)
	}

	// The Personal e-mail was the primary: the new one becomes it, unproven.
	if err := f.svc.ChangeEmail(ctx, p, f.member, identity.EmailChange{Address: " Yeni@Example.com "}); err != nil {
		t.Fatal(err)
	}
	u, _ := f.accounts.Get(f.member)
	if u.Email != "yeni@example.com" || u.EmailVerified || u.Attributes["personalEmail"][0] != "yeni@example.com" || u.Attributes["personalEmailVerifiedAt"] != nil {
		t.Fatalf("after change = %+v", u)
	}
	if u.Attributes["schoolEmail"][0] != "ada@std.yildiz.edu.tr" {
		t.Fatal("school e-mail changed")
	}
	// Vouched for, and the school address primary: only the personal one moves.
	if err := f.svc.SetPrimary(ctx, p, f.member, identity.PrimarySchool); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangeEmail(ctx, p, f.member, identity.EmailChange{Address: "ikinci@example.com", Verified: true}); err != nil {
		t.Fatal(err)
	}
	u, _ = f.accounts.Get(f.member)
	if u.Email != "ada@std.yildiz.edu.tr" || !u.EmailVerified || u.Attributes["personalEmailVerifiedAt"][0] != "2026-11-22T21:00:00Z" {
		t.Fatalf("vouched = %+v", u)
	}
	for _, row := range f.audit.Rows() {
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), "@") {
			t.Fatalf("a row holds an address: %s", raw)
		}
	}
}

func TestVerifyAndPrimary(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	ctx := accountActor(p)
	u, _ := f.accounts.Get(f.member)
	u.EmailVerified = false
	u.Attributes = map[string][]string{"personalEmail": {"ada@example.com"}}
	f.accounts.PutAccount(u)
	if err := f.svc.SetPrimary(ctx, p, f.member, identity.PrimaryPersonal); accountCode(err) != identity.AccountEmailUnverified {
		t.Fatalf("unproven personal primary: %v", err)
	}
	if err := f.svc.SetPrimary(ctx, p, f.member, identity.PrimarySchool); accountCode(err) != identity.AccountNoAddress {
		t.Fatalf("no school e-mail: %v", err)
	}
	if err := f.svc.VerifyEmail(ctx, p, f.member); err != nil {
		t.Fatal(err)
	}
	u, _ = f.accounts.Get(f.member)
	if !u.EmailVerified || u.Attributes["personalEmailVerifiedAt"][0] != "2026-11-22T21:00:00Z" {
		t.Fatalf("verified = %+v", u)
	}
	// A primary from before the two e-mails becomes the proven Personal e-mail.
	u.Email = "eski@example.com"
	u.EmailVerified = false
	u.Attributes = map[string][]string{}
	f.accounts.PutAccount(u)
	if err := f.svc.VerifyEmail(ctx, p, f.member); err != nil {
		t.Fatal(err)
	}
	u, _ = f.accounts.Get(f.member)
	if !u.EmailVerified || u.Attributes["personalEmail"][0] != "eski@example.com" {
		t.Fatalf("adopted = %+v", u)
	}
	if err := f.svc.SetPrimary(ctx, p, f.member, "other"); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("kind: %v", err)
	}
}

func TestRequiredActionsKeepTheOthers(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	u, _ := f.accounts.Get(f.member)
	u.RequiredActions = []string{"TERMS_AND_CONDITIONS", "CONFIGURE_TOTP"}
	f.accounts.PutAccount(u)
	if err := f.svc.SetRequiredActions(accountActor(p), p, f.member, []string{"delete_account"}); accountCode(err) != identity.AccountUnknownAction {
		t.Fatalf("unknown: %v", err)
	}
	if err := f.svc.SetRequiredActions(accountActor(p), p, f.member, []string{"UPDATE_PASSWORD", "VERIFY_EMAIL"}); err != nil {
		t.Fatal(err)
	}
	u, _ = f.accounts.Get(f.member)
	if !slices.Equal(u.RequiredActions, []string{"TERMS_AND_CONDITIONS", "UPDATE_PASSWORD", "VERIFY_EMAIL"}) {
		t.Fatalf("actions = %v", u.RequiredActions)
	}
	row := f.audit.Rows()[0]
	if row.Action != teamaudit.ActionUserRequiredActions || !slices.Contains(row.After.RequiredActions, "VERIFY_EMAIL") || !slices.Contains(row.Before.RequiredActions, "CONFIGURE_TOTP") {
		t.Fatalf("row = %+v", row)
	}
}

func TestCredentialsAndLinksAreRemovedOneByOne(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	passkey, otp := uuid.NewString(), uuid.NewString()
	f.accounts.PutCredentials(f.member, identity.AccountCredential{ID: passkey, Type: "webauthn-passwordless"}, identity.AccountCredential{ID: otp, Type: "otp"})
	f.accounts.PutLinks(f.member, identity.LinkedIdentity{Provider: "OBS"})
	if err := f.svc.DeleteCredential(accountActor(p), p, f.member, uuid.NewString()); accountCode(err) != identity.AccountNoCredential {
		t.Fatalf("unknown credential: %v", err)
	}
	if err := f.svc.DeleteCredential(accountActor(p), p, f.member, passkey); err != nil {
		t.Fatal(err)
	}
	left, _ := f.accounts.AccountCredentials(context.Background(), f.member)
	if len(left) != 1 || left[0].ID != otp {
		t.Fatalf("left = %+v", left)
	}
	if err := f.svc.UnlinkIdentity(accountActor(p), p, f.member, "GOOGLE"); accountCode(err) != identity.AccountNoProvider {
		t.Fatalf("unknown provider: %v", err)
	}
	if err := f.svc.UnlinkIdentity(accountActor(p), p, f.member, "OBS"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ClearLockout(accountActor(p), p, f.member); err != nil {
		t.Fatal(err)
	}
	var actions []teamaudit.Action
	for _, row := range f.audit.Rows() {
		actions = append(actions, row.Action)
	}
	if !slices.Equal(actions, []teamaudit.Action{teamaudit.ActionUserCredentialRemove, teamaudit.ActionUserIdentityUnlink, teamaudit.ActionUserLockoutClear}) {
		t.Fatalf("actions = %v", actions)
	}
	if row := f.audit.Rows()[0]; row.Before.CredentialType != "webauthn-passwordless" || row.Before.CredentialID != passkey {
		t.Fatalf("row = %+v", row)
	}
}

func TestErasedAndServiceAccountsAreNotChanged(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	if _, _, err := f.users.Upsert(context.Background(), user.User{ID: f.member, Email: "ada@example.com", AccountState: user.AccountActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.users.RequestDeletion(context.Background(), f.member, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetEnabled(accountActor(p), p, f.member, true); accountCode(err) != identity.AccountNotActive {
		t.Fatalf("pending deletion: %v", err)
	}
	account, err := f.svc.Account(context.Background(), p, f.member)
	if err != nil || account.Status != user.ReadStatusDeletionPending || account.Email != nil || account.Can.Manage {
		t.Fatalf("read = %+v %v", account, err)
	}

	robot := uuid.New()
	f.accounts.PutAccount(identity.AccountUser{ID: robot, Username: "service-account-forms", Enabled: true, ServiceAccount: true})
	if err := f.svc.SetEnabled(accountActor(p), p, robot, false); accountCode(err) != identity.AccountServiceAccount {
		t.Fatalf("service account: %v", err)
	}
}

func TestWriteNamesGoesToKeycloak(t *testing.T) {
	f := newAccountsFixture(t)
	p := asYK(f)
	first, last := " Ayşe ", "Lovelace"
	if err := f.svc.WriteNames(accountActor(p), p, f.member, identity.NameChange{FirstName: &first, LastName: &last}); err != nil {
		t.Fatal(err)
	}
	u, _ := f.accounts.Get(f.member)
	if u.FirstName != "Ayşe" || u.LastName != "Lovelace" {
		t.Fatalf("names = %+v", u)
	}
	rows := f.audit.Rows()
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionUserProfile || !slices.Equal(rows[0].After.Fields, []string{"firstName"}) {
		t.Fatalf("rows = %+v", rows)
	}
	raw, _ := json.Marshal(rows[0])
	if strings.Contains(string(raw), "Ayşe") {
		t.Fatalf("a row holds the name: %s", raw)
	}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if err := f.svc.WriteNames(accountActor(member), member, f.member, identity.NameChange{FirstName: &first}); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
}
