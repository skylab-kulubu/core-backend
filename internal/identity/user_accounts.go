package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A person's Keycloak account as the panel's person card reads and changes
// it (docs/user-accounts.md): the sign-in switch, the e-mail addresses
// (ADR-0044's School, Personal and Primary e-mail), passwords, required
// actions, credentials, linked identity providers, sessions, the brute-force
// lock and the recent sign-ins. Every write goes through core's authorizer
// and the Team audit record; none is made on the caller's own account (the
// Account center does that after Sudo mode, ADR-0043).

// AccountDirectory is the identity provider's account side of a person.
// Keycloak implements it; MemoryAccounts stands in for tests and local runs.
type AccountDirectory interface {
	AccountUser(ctx context.Context, id uuid.UUID) (AccountUser, error)
	AccountCredentials(ctx context.Context, id uuid.UUID) ([]AccountCredential, error)
	LinkedIdentities(ctx context.Context, id uuid.UUID) ([]LinkedIdentity, error)
	AccountSessions(ctx context.Context, id uuid.UUID) ([]AccountSession, error)
	Lockout(ctx context.Context, id uuid.UUID) (AccountLockout, error)
	// LoginEvents are the person's newest sign-in events, at most max.
	// ErrEventsUnavailable: the directory may not read them.
	LoginEvents(ctx context.Context, id uuid.UUID, limit int) ([]LoginEvent, error)
	// AddressHolders are the accounts that hold address as their Primary,
	// School or Personal e-mail (address trimmed and lower-cased).
	AddressHolders(ctx context.Context, address string) ([]uuid.UUID, error)
	SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error
	// SetTemporaryPassword sets password as a temporary one: the person
	// changes it at the next sign-in. No error names the password.
	SetTemporaryPassword(ctx context.Context, id uuid.UUID, password string) error
	SendActionsMail(ctx context.Context, id uuid.UUID, actions []string) error
	SendVerificationMail(ctx context.Context, id uuid.UUID) error
	UpdateAccount(ctx context.Context, id uuid.UUID, change AccountUpdate) error
	DeleteCredential(ctx context.Context, id uuid.UUID, credentialID string) error
	UnlinkIdentity(ctx context.Context, id uuid.UUID, provider string) error
	ClearLockout(ctx context.Context, id uuid.UUID) error
	LogoutAllSessions(ctx context.Context, id uuid.UUID) error
}

// ErrEventsUnavailable is a directory that may not read sign-in events
// (Keycloak without view-events): the card says so, nothing fails.
var ErrEventsUnavailable = errors.New("identity: sign-in events cannot be read")

// AccountUser is the account as the identity provider holds it.
type AccountUser struct {
	ID              uuid.UUID
	Username        string
	Email           string
	EmailVerified   bool
	FirstName       string
	LastName        string
	Enabled         bool
	ServiceAccount  bool
	CreatedAt       *time.Time
	RequiredActions []string
	Attributes      map[string][]string
}

func (u AccountUser) attribute(name string) string {
	if values := u.Attributes[name]; len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

// AccountUpdate is one write of the account: each field set is written,
// the others stay. Attributes sets an attribute to its value, or removes
// it when the value is empty; only the Personal e-mail's two attributes are
// written this way.
type AccountUpdate struct {
	FirstName       *string
	LastName        *string
	Email           *string
	EmailVerified   *bool
	RequiredActions *[]string
	Attributes      map[string]string
}

// touchesProfile reports whether the write changes the User Profile's
// attributes (names, e-mail, attributes): Keycloak then needs them whole.
func (u AccountUpdate) touchesProfile() bool {
	return u.FirstName != nil || u.LastName != nil || u.Email != nil || len(u.Attributes) > 0
}

// AccountCredential is one credential of the person: its id, type
// (password, otp, webauthn, webauthn-passwordless) and the person's label.
// Its secret data is never read out.
type AccountCredential struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Label     string     `json:"label,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// LinkedIdentity is a link to an identity provider: its alias, the
// person's user name there, and whether it is the YTÜ Microsoft one.
type LinkedIdentity struct {
	Provider string `json:"provider"`
	UserName string `json:"userName,omitempty"`
	YTU      bool   `json:"ytu"`
}

// AccountSession is one sign-in session: when it started, when it was last
// used and the applications in it. Its address is never read out.
type AccountSession struct {
	Start      time.Time `json:"start"`
	LastAccess time.Time `json:"lastAccess"`
	Clients    []string  `json:"clients"`
}

// AccountLockout is the brute-force protection's state of the account.
type AccountLockout struct {
	Locked      bool       `json:"locked"`
	Failures    int        `json:"failures"`
	LastFailure *time.Time `json:"lastFailure,omitempty"`
}

// LoginEvent is one sign-in event: its type (LOGIN, LOGIN_ERROR …), time,
// application and error code. Its address is never read out.
type LoginEvent struct {
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Client string    `json:"client,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// Account is GET /v1/users/{id}/account.
type Account struct {
	ID     uuid.UUID       `json:"id"`
	Status user.ReadStatus `json:"status"`
	// The rest is absent for a person who is erased or being erased.
	Username          string              `json:"username,omitempty"`
	Enabled           bool                `json:"enabled"`
	CreatedAt         *time.Time          `json:"createdAt,omitempty"`
	Email             *AccountEmail       `json:"email,omitempty"`
	YTUVerified       bool                `json:"ytuVerified"`
	RequiredActions   []string            `json:"requiredActions"`
	Credentials       []AccountCredential `json:"credentials"`
	IdentityProviders []LinkedIdentity    `json:"identityProviders"`
	Sessions          []AccountSession    `json:"sessions"`
	Lockout           AccountLockout      `json:"lockout"`
	// Logins is nil when the sign-in events cannot be read.
	Logins *AccountLogins `json:"logins"`
	Can    AccountCan     `json:"can"`
}

// AccountEmail are the person's addresses (ADR-0044): the Primary e-mail
// with Keycloak's flag and which of the two it is (school, personal or
// none), the School e-mail and the Personal e-mail with whether it is
// proven.
type AccountEmail struct {
	Primary          string `json:"primary,omitempty"`
	PrimaryVerified  bool   `json:"primaryVerified"`
	PrimaryKind      string `json:"primaryKind"`
	School           string `json:"school,omitempty"`
	Personal         string `json:"personal,omitempty"`
	PersonalVerified bool   `json:"personalVerified"`
}

// AccountLogins are the recent sign-in events, newest first, and the time
// of the last successful one in them.
type AccountLogins struct {
	Last   *time.Time   `json:"last,omitempty"`
	Events []LoginEvent `json:"events"`
}

// AccountCan says whether the caller may change the account here, and why
// not (an AccountError code).
type AccountCan struct {
	Manage bool   `json:"manage"`
	Reason string `json:"reason,omitempty"`
}

// The primary kinds.
const (
	PrimarySchool   = "school"
	PrimaryPersonal = "personal"
	PrimaryNone     = "none"
)

// RequiredActionAliases are the required actions the panel may set: the
// realm's own (e-skylab-keycloak config/account-center-required-actions.tsv)
// and Keycloak's e-mail verification. Others the account carries stay as
// they are.
var RequiredActionAliases = []string{"UPDATE_PASSWORD", "CONFIGURE_TOTP", "webauthn-register-passwordless", "VERIFY_EMAIL"}

// removableCredentialTypes are the credential types the panel removes.
var removableCredentialTypes = []string{"password", "otp", "webauthn", "webauthn-passwordless"}

// AccountError is a refused account request: its HTTP status and problem
// code, and for a password the realm's policy message key.
type AccountError struct {
	Status int
	Code   string
	Policy string
}

func (e *AccountError) Error() string { return "identity: account: " + e.Code }

func accountError(status int, code string) error { return &AccountError{Status: status, Code: code} }

// The account refusals.
const (
	AccountOwn              = "own_account"
	AccountPrivilegedTarget = "privileged_target"
	AccountServiceAccount   = "service_account"
	AccountNotActive        = "account_not_active"
	AccountSchoolDomain     = "school_domain"
	AccountEmailInvalid     = "email_invalid"
	AccountEmailTaken       = "email_taken"
	AccountEmailUnverified  = "email_not_verified"
	AccountNoAddress        = "no_address"
	AccountNoEmail          = "no_email"
	AccountPasswordPolicy   = "password_policy"
	AccountUnknownAction    = "unknown_required_action"
	AccountNoCredential     = "credential_not_found"
	AccountNoProvider       = "provider_not_found"
	AccountMailFailed       = "mail_not_sent"
	AccountNameInvalid      = "name_invalid"
	AccountInvalid          = "account_invalid"
)

// UserAccountOptions are what UserAccounts reads and writes.
type UserAccountOptions struct {
	// Accounts is the identity provider's account side.
	Accounts AccountDirectory
	// People reads the target's Groups (Privileged or not).
	People Directory
	Users  user.Store
	Authz  authz.Authorizer
	// PrivilegedRoots are the Privileged roots (AUTHZ_PRIVILEGED_ROOTS):
	// an account in one, or under one, is changed only by someone in the
	// ADMIN root. Nil is authz.DefaultPrivilegedRoots.
	PrivilegedRoots []string
	// Audit records every write and every refused one. Required.
	Audit teamaudit.Writer
	// YTUProvider is the YTÜ Microsoft identity provider's alias
	// (KEYCLOAK_YTU_IDP_ALIAS).
	YTUProvider string
	Now         func() time.Time
	Logf        func(string, ...any)
}

// UserAccounts reads and changes people's accounts for the person card.
type UserAccounts struct {
	o UserAccountOptions

	mu sync.Mutex
	// denied is when a denied row was last written for a caller, target,
	// action and code: a refusal repeated within deniedWindow writes no
	// second row.
	denied map[string]time.Time
}

// deniedWindow is how long a repeated refusal writes no new denied row.
const deniedWindow = time.Minute

// NewUserAccounts is the account service over o.
func NewUserAccounts(o UserAccountOptions) *UserAccounts {
	if o.PrivilegedRoots == nil {
		o.PrivilegedRoots = authz.DefaultPrivilegedRoots()
	}
	if o.YTUProvider == "" {
		o.YTUProvider = "OBS"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return &UserAccounts{o: o, denied: map[string]time.Time{}}
}

// loginEventPage is how many sign-in events the card shows.
const loginEventPage = 10

// Account reads the person's account: Privileged readers (DK) and
// users:manage holders.
func (s *UserAccounts) Account(ctx context.Context, p authz.Principal, id uuid.UUID) (Account, error) {
	if !s.o.Authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.ReadFull) {
		return Account{}, ErrForbidden
	}
	if status, err := s.coreStatus(ctx, id); err != nil {
		return Account{}, err
	} else if status != user.ReadStatusActive {
		return Account{ID: id, Status: status, RequiredActions: []string{}, Credentials: []AccountCredential{},
			IdentityProviders: []LinkedIdentity{}, Sessions: []AccountSession{}, Can: AccountCan{Reason: AccountNotActive}}, nil
	}
	u, err := s.o.Accounts.AccountUser(ctx, id)
	if err != nil {
		return Account{}, err
	}
	credentials, err := s.o.Accounts.AccountCredentials(ctx, id)
	if err != nil {
		return Account{}, err
	}
	links, err := s.o.Accounts.LinkedIdentities(ctx, id)
	if err != nil {
		return Account{}, err
	}
	sessions, err := s.o.Accounts.AccountSessions(ctx, id)
	if err != nil {
		return Account{}, err
	}
	lockout, err := s.o.Accounts.Lockout(ctx, id)
	if err != nil {
		return Account{}, err
	}
	account := Account{
		ID: id, Status: user.ReadStatusActive, Username: u.Username, Enabled: u.Enabled, CreatedAt: u.CreatedAt,
		Email:           accountEmail(u),
		RequiredActions: nonNil(u.RequiredActions),
		Credentials:     nonNil(credentials),
		Sessions:        nonNil(sessions),
		Lockout:         lockout,
	}
	account.IdentityProviders = make([]LinkedIdentity, 0, len(links))
	for _, link := range links {
		link.YTU = link.Provider == s.o.YTUProvider
		account.YTUVerified = account.YTUVerified || link.YTU
		account.IdentityProviders = append(account.IdentityProviders, link)
	}
	events, err := s.o.Accounts.LoginEvents(ctx, id, loginEventPage)
	switch {
	case errors.Is(err, ErrEventsUnavailable):
	case err != nil:
		s.o.Logf("user accounts: sign-in events could not be read: %v", err)
	default:
		logins := &AccountLogins{Events: nonNil(events)}
		for _, event := range events {
			if event.Type == "LOGIN" {
				at := event.Time
				logins.Last = &at
				break
			}
		}
		account.Logins = logins
	}
	account.Can = s.can(ctx, p, u)
	return account, nil
}

func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// accountEmail reads the addresses as sky-account does (GET identity): the
// primary is school or personal when Keycloak's email is that address,
// whatever the case, none otherwise; the Personal e-mail is proven only with
// its stamp.
func accountEmail(u AccountUser) *AccountEmail {
	school := u.attribute(schoolEmailAttribute)
	personal := u.attribute(personalEmailAttribute)
	out := &AccountEmail{
		Primary: u.Email, PrimaryVerified: u.EmailVerified, PrimaryKind: PrimaryNone,
		School: school, Personal: personal,
		PersonalVerified: personal != "" && u.attribute(personalEmailVerifiedAtAttribute) != "",
	}
	switch {
	case u.Email == "":
	case school != "" && strings.EqualFold(u.Email, school):
		out.PrimaryKind = PrimarySchool
	case personal != "" && strings.EqualFold(u.Email, personal):
		out.PrimaryKind = PrimaryPersonal
	}
	return out
}

// coreStatus is the person's status as core's own row says: active when
// core has no row and no deletion marker.
func (s *UserAccounts) coreStatus(ctx context.Context, id uuid.UUID) (user.ReadStatus, error) {
	if id == user.DeletedSubject {
		return user.ReadStatusDeleted, nil
	}
	shadow, err := s.o.Users.Get(ctx, id)
	switch {
	case err == nil:
		return shadow.AccountState.ReadStatus(), nil
	case errors.Is(err, user.ErrNotFound):
		state, err := s.o.Users.AttributionState(ctx, id)
		if err != nil {
			return "", err
		}
		if state == user.AttributionBlocked {
			return user.ReadStatusDeleted, nil
		}
		return user.ReadStatusActive, nil
	default:
		return "", err
	}
}

// can is AccountCan for the card: what write would refuse, without a
// record.
func (s *UserAccounts) can(ctx context.Context, p authz.Principal, u AccountUser) AccountCan {
	if !s.o.Authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
		return AccountCan{Reason: "forbidden"}
	}
	if code := s.refusal(ctx, p, u); code != "" {
		return AccountCan{Reason: code}
	}
	return AccountCan{Manage: true}
}

// refusal is why the caller may not change u's account at all, or "".
func (s *UserAccounts) refusal(ctx context.Context, p authz.Principal, u AccountUser) string {
	if p.ID == u.ID.String() {
		return AccountOwn
	}
	if u.ServiceAccount {
		return AccountServiceAccount
	}
	privileged, err := s.privilegedTarget(ctx, u.ID)
	if err != nil || (privileged && !s.callerIsAdmin(p)) {
		return AccountPrivilegedTarget
	}
	return ""
}

// HeldRoleReader reads a person's roles in effect, as Keycloak resolves
// them: every realm role and every client role the person holds directly,
// through their Groups and the Groups above them, through the default role
// and through composites at any depth (Keycloak's role-mappings/…/composite).
// A realm role has an empty ClientID. The Keycloak and the memory
// directories have it.
type HeldRoleReader interface {
	HeldRoles(ctx context.Context, id uuid.UUID) ([]ClientRole, error)
}

var (
	_ HeldRoleReader = (*Keycloak)(nil)
	_ HeldRoleReader = (*Memory)(nil)
)

// everyonesRealmRoles are the realm roles that make nobody privileged: the
// default role's Keycloak parts and the Java era's USER, which /UYELER
// holds until the role cleanup's F3 (ADR-0076). default-roles-<realm> is
// matched by its prefix.
var everyonesRealmRoles = []string{"offline_access", "uma_authorization", "USER"}

// privilegedRole reports whether a role the person holds in effect is a
// privilege, as Entra counts one (only a Privileged Authentication
// Administrator resets the credentials of any admin role's holder): every
// realm role but everyone's, and every client role that is no app role
// (authz.ClassifyRole: an admin role or one nobody has classified). The
// account client's roles (one's own account) and broker/read-token (one's
// own identity provider token, which a first login once gave and the role
// cleanup takes) are nobody's privilege.
func privilegedRole(role ClientRole) bool {
	switch {
	case role.ClientID == "":
		return !strings.HasPrefix(role.Role, "default-roles-") && !slices.Contains(everyonesRealmRoles, role.Role)
	case role.ClientID == "account":
		return false
	case role.ClientID == "broker" && role.Role == "read-token":
		return false
	}
	return authz.ClassifyRole(role.ClientID, role.Role) != authz.RoleClassApp
}

// privilegedTarget reports whether the person is privileged: in a
// Privileged root (ADMIN, YK, DK, or a Group under one), or holding in
// effect a role privilegedRole counts (FORMS-ALL, SkyMail's sending,
// approving and reading roles, place:admin, a Java era realm role or the
// admin role it holds as a composite, a role nobody has classified …).
// Keycloak resolves the roles: Groups above the person's, realm and client
// composites and the default role included. A directory that cannot read
// them answers privileged: only ADMIN changes the account.
func (s *UserAccounts) privilegedTarget(ctx context.Context, id uuid.UUID) (bool, error) {
	groups, err := s.o.People.GroupsForUser(ctx, id)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if authz.PrivilegedRoot(s.o.PrivilegedRoots, g.Path) != "" {
			return true, nil
		}
	}
	reader, ok := s.o.People.(HeldRoleReader)
	if !ok {
		return true, nil
	}
	held, err := reader.HeldRoles(ctx, id)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(held, privilegedRole), nil
}

// callerIsAdmin reports whether the caller is in the ADMIN root (itself or
// a subgroup), read from the token's Groups.
func (s *UserAccounts) callerIsAdmin(p authz.Principal) bool {
	for _, g := range p.Groups {
		if root := authz.PrivilegedRoot(s.o.PrivilegedRoots, g); root != "" && authz.RootLabel(root) == "ADMIN" {
			return true
		}
	}
	return false
}

// target decides a write of action on id's account and reads the account:
// users:manage (ADMIN and YK; DK never), not the caller's own account, not
// a service account, not a person who is erased or being erased, and for
// a Privileged person only a caller in the ADMIN root. A refusal of the
// caller (forbidden, own account, Privileged target) is a denied row.
func (s *UserAccounts) target(ctx context.Context, p authz.Principal, id uuid.UUID, action teamaudit.Action) (AccountUser, error) {
	if !s.o.Authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
		s.deny(ctx, p, id, action, "forbidden")
		return AccountUser{}, ErrForbidden
	}
	if p.ID == id.String() {
		s.deny(ctx, p, id, action, AccountOwn)
		return AccountUser{}, accountError(409, AccountOwn)
	}
	status, err := s.coreStatus(ctx, id)
	if err != nil {
		return AccountUser{}, err
	}
	if status != user.ReadStatusActive {
		return AccountUser{}, accountError(409, AccountNotActive)
	}
	u, err := s.o.Accounts.AccountUser(ctx, id)
	if err != nil {
		return AccountUser{}, err
	}
	if u.ServiceAccount {
		return AccountUser{}, accountError(409, AccountServiceAccount)
	}
	privileged, err := s.privilegedTarget(ctx, id)
	if err != nil {
		return AccountUser{}, err
	}
	if privileged && !s.callerIsAdmin(p) {
		s.deny(ctx, p, id, action, AccountPrivilegedTarget)
		return AccountUser{}, accountError(403, AccountPrivilegedTarget)
	}
	return u, nil
}

// deny writes a denied row; a row that cannot be written is logged, the
// refusal stands.
func (s *UserAccounts) deny(ctx context.Context, p authz.Principal, id uuid.UUID, action teamaudit.Action, code string) {
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok {
		return
	}
	// The same refusal again within deniedWindow is the first one's row: a
	// caller cannot fill the record (kept three years) by repeating it.
	// A caller with no right at all is one refusal whatever the person:
	// naming each person would let random ids fill the record anyway.
	target := id.String()
	if code == "forbidden" {
		target = "*"
	}
	key := actor.ID.String() + "|" + target + "|" + string(action) + "|" + code
	now := s.o.Now()
	s.mu.Lock()
	if last, seen := s.denied[key]; seen && now.Sub(last) < deniedWindow {
		s.mu.Unlock()
		return
	}
	if len(s.denied) > 4096 {
		for k, at := range s.denied {
			if now.Sub(at) >= deniedWindow {
				delete(s.denied, k)
			}
		}
	}
	s.denied[key] = now
	s.mu.Unlock()
	actor.Basis = teamaudit.BasisNone
	actor.ViaGroupID = ""
	if err := s.o.Audit.Deny(ctx, teamaudit.Record{Actor: actor, Action: action, TargetUserID: id}, code); err != nil {
		s.o.Logf("user accounts: denied row not written: %v", err)
	}
}

// write records do as action on id: a pending row, the write, its outcome.
func (s *UserAccounts) write(ctx context.Context, id uuid.UUID, action teamaudit.Action, before, after *teamaudit.State, do func() error) error {
	return recordWrite(ctx, s.o.Audit, s.o.Logf, teamaudit.Record{
		Action: action, TargetUserID: id, Before: before, After: after,
	}, func() (*teamaudit.State, error) { return nil, do() })
}

func flag(b bool) *bool { return &b }

// SetEnabled turns the account's sign-in on or off. Turning it off also
// ends every session; the account stays, with everything in it.
func (s *UserAccounts) SetEnabled(ctx context.Context, p authz.Principal, id uuid.UUID, enabled bool) error {
	action := teamaudit.ActionUserDisable
	if enabled {
		action = teamaudit.ActionUserEnable
	}
	u, err := s.target(ctx, p, id, action)
	if err != nil {
		return err
	}
	if u.Enabled == enabled {
		return nil
	}
	if err := s.write(ctx, id, action, &teamaudit.State{Enabled: flag(u.Enabled)}, &teamaudit.State{Enabled: flag(enabled)}, func() error {
		return s.o.Accounts.SetEnabled(ctx, id, enabled)
	}); err != nil {
		return err
	}
	if !enabled {
		if err := s.o.Accounts.LogoutAllSessions(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
			s.o.Logf("user accounts: sessions not ended after disable: %v", err)
		}
	}
	return nil
}

// PasswordResult is POST /v1/users/{id}/password: the password core made up,
// shown once, and whether every session was ended.
type PasswordResult struct {
	Generated      bool   `json:"generated"`
	Password       string `json:"password,omitempty"`
	Temporary      bool   `json:"temporary"`
	SessionsClosed bool   `json:"sessionsClosed"`
}

// generatedPasswordAlphabet leaves out the characters read alike (0 O o,
// 1 l I).
const generatedPasswordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword makes up a temporary password: four groups of four,
// e.g. hT7k-Pq3m-Zx9r-Wd2n (about 92 bits).
func GeneratePassword() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(generatedPasswordAlphabet)))
	for i := range 16 {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(generatedPasswordAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// maxPasswordLength bounds a password the caller types.
const maxPasswordLength = 128

// SetTemporaryPassword sets a temporary password: the one given, or one
// core makes up when password is empty, which the answer carries once. The
// person changes it at the next sign-in, and every session ends. The
// password goes to Keycloak alone: no log, record or error holds it.
func (s *UserAccounts) SetTemporaryPassword(ctx context.Context, p authz.Principal, id uuid.UUID, password string) (PasswordResult, error) {
	if _, err := s.target(ctx, p, id, teamaudit.ActionUserPasswordSet); err != nil {
		return PasswordResult{}, err
	}
	if len(password) > maxPasswordLength || (password != "" && strings.TrimSpace(password) == "") {
		return PasswordResult{}, accountError(422, AccountPasswordPolicy)
	}
	generated := password == ""
	if generated {
		var err error
		if password, err = GeneratePassword(); err != nil {
			return PasswordResult{}, err
		}
	}
	if err := s.write(ctx, id, teamaudit.ActionUserPasswordSet, nil,
		&teamaudit.State{Temporary: flag(true), Generated: flag(generated)}, func() error {
			return s.o.Accounts.SetTemporaryPassword(ctx, id, password)
		}); err != nil {
		return PasswordResult{}, err
	}
	result := PasswordResult{Generated: generated, Temporary: true, SessionsClosed: true}
	if generated {
		result.Password = password
	}
	if err := s.o.Accounts.LogoutAllSessions(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		s.o.Logf("user accounts: sessions not ended after a password was set: %v", err)
		result.SessionsClosed = false
	}
	return result, nil
}

// SendPasswordMail mails the person a link to choose a new password
// (Keycloak's execute-actions mail, UPDATE_PASSWORD).
func (s *UserAccounts) SendPasswordMail(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserPasswordMail)
	if err != nil {
		return err
	}
	if u.Email == "" {
		return accountError(409, AccountNoEmail)
	}
	return s.write(ctx, id, teamaudit.ActionUserPasswordMail, nil,
		&teamaudit.State{RequiredActions: []string{"UPDATE_PASSWORD"}}, func() error {
			return s.o.Accounts.SendActionsMail(ctx, id, []string{"UPDATE_PASSWORD"})
		})
}

// SendVerificationMail mails the Primary e-mail a link that proves it.
func (s *UserAccounts) SendVerificationMail(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserEmailMail)
	if err != nil {
		return err
	}
	if u.Email == "" {
		return accountError(409, AccountNoEmail)
	}
	return s.write(ctx, id, teamaudit.ActionUserEmailMail, nil, nil, func() error {
		return s.o.Accounts.SendVerificationMail(ctx, id)
	})
}

// EmailChange is PUT /v1/users/{id}/email: the new Personal e-mail, whether
// the caller vouches for it, and whether it becomes the Primary e-mail.
type EmailChange struct {
	Address  string
	Verified bool
	Primary  bool
}

// NormalizeAddress trims and lower-cases an address as sky-account does
// (PersonalEmailProof.normalise).
func NormalizeAddress(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// maxAddressLength is an address's longest form (RFC 5321).
const maxAddressLength = 254

// IsSchoolDomain reports whether the address is on a YTÜ school domain
// (yildiz.edu.tr, std.yildiz.edu.tr or a subdomain): never a Personal
// e-mail (ADR-0063).
func IsSchoolDomain(address string) bool {
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return false
	}
	domain := strings.ToLower(address[at+1:])
	return domain == "yildiz.edu.tr" || strings.HasSuffix(domain, ".yildiz.edu.tr")
}

func validAddress(address string) bool {
	if address == "" || len(address) > maxAddressLength || strings.ContainsAny(address, " \t\r\n:,;<>\"") {
		return false
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	at := strings.LastIndexByte(address, '@')
	return at > 0 && strings.Contains(address[at+1:], ".")
}

// ChangeEmail gives the person a new Personal e-mail (ADR-0044): proven when
// the caller vouches for it, otherwise not, and the Primary e-mail too
// when asked. An address on a school domain or held by another account is
// refused. The School e-mail comes from the YTÜ link alone and is never
// written here.
func (s *UserAccounts) ChangeEmail(ctx context.Context, p authz.Principal, id uuid.UUID, in EmailChange) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserEmailChange)
	if err != nil {
		return err
	}
	address := NormalizeAddress(in.Address)
	switch {
	case !validAddress(address):
		return accountError(422, AccountEmailInvalid)
	case IsSchoolDomain(address):
		return accountError(422, AccountSchoolDomain)
	}
	holders, err := s.o.Accounts.AddressHolders(ctx, address)
	if err != nil {
		return err
	}
	for _, holder := range holders {
		if holder != id {
			return accountError(409, AccountEmailTaken)
		}
	}
	before := accountEmail(u)
	change := AccountUpdate{Attributes: map[string]string{personalEmailAttribute: address, personalEmailVerifiedAtAttribute: ""}}
	if in.Verified {
		change.Attributes[personalEmailVerifiedAtAttribute] = s.o.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	// The old Personal e-mail was the primary: the person would keep a
	// primary they no longer own, so the new one follows (sky-account's
	// email/confirm does the same).
	primary := in.Primary || before.PrimaryKind == PrimaryPersonal || u.Email == ""
	afterKind := before.PrimaryKind
	if primary {
		change.Email = &address
		change.EmailVerified = flag(in.Verified)
		afterKind = PrimaryPersonal
	}
	return s.write(ctx, id, teamaudit.ActionUserEmailChange,
		&teamaudit.State{Primary: before.PrimaryKind, Verified: flag(before.PersonalVerified)},
		&teamaudit.State{Primary: afterKind, Verified: flag(in.Verified), Fields: []string{"personalEmail"}},
		func() error { return s.o.Accounts.UpdateAccount(ctx, id, change) })
}

// VerifyEmail marks an address proven, on the caller's word: the Primary
// e-mail (kind "" or "primary"), and a Personal e-mail that is the primary
// gets its proof stamp too; or the Personal e-mail (kind "personal"),
// which gets its stamp, and Keycloak's flag when it is the primary. That
// also stamps a Personal primary that a verification link proved, which
// leaves Keycloak's flag set and no stamp.
func (s *UserAccounts) VerifyEmail(ctx context.Context, p authz.Principal, id uuid.UUID, kind string) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserEmailVerify)
	if err != nil {
		return err
	}
	switch kind {
	case "", "primary", PrimaryPersonal:
	default:
		return ErrInvalid
	}
	email := accountEmail(u)
	stamp := s.o.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	change := AccountUpdate{}
	if kind == PrimaryPersonal {
		if email.Personal == "" {
			return accountError(409, AccountNoAddress)
		}
		if email.PersonalVerified && (email.PrimaryKind != PrimaryPersonal || u.EmailVerified) {
			return nil
		}
		change.Attributes = map[string]string{
			personalEmailAttribute:           NormalizeAddress(email.Personal),
			personalEmailVerifiedAtAttribute: stamp,
		}
		if email.PrimaryKind == PrimaryPersonal {
			change.EmailVerified = flag(true)
		}
		return s.write(ctx, id, teamaudit.ActionUserEmailVerify,
			&teamaudit.State{Primary: PrimaryPersonal, Verified: flag(email.PersonalVerified)},
			&teamaudit.State{Primary: PrimaryPersonal, Verified: flag(true)},
			func() error { return s.o.Accounts.UpdateAccount(ctx, id, change) })
	}
	if u.Email == "" {
		return accountError(409, AccountNoEmail)
	}
	change.EmailVerified = flag(true)
	switch {
	case email.PrimaryKind == PrimaryPersonal && !email.PersonalVerified:
		change.Attributes = map[string]string{
			personalEmailAttribute:           NormalizeAddress(email.Personal),
			personalEmailVerifiedAtAttribute: stamp,
		}
	case email.PrimaryKind == PrimaryNone && email.Personal == "" && !IsSchoolDomain(u.Email):
		// A primary from before the two e-mails, proven now: it becomes the
		// Personal e-mail, as sky-account's email/confirm does (A1c), unless
		// another account holds the address (then only the flag is set).
		holders, err := s.o.Accounts.AddressHolders(ctx, NormalizeAddress(u.Email))
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(holders, func(h uuid.UUID) bool { return h != id }) {
			change.Attributes = map[string]string{
				personalEmailAttribute:           NormalizeAddress(u.Email),
				personalEmailVerifiedAtAttribute: stamp,
			}
		}
	}
	if u.EmailVerified && change.Attributes == nil {
		return nil
	}
	return s.write(ctx, id, teamaudit.ActionUserEmailVerify,
		&teamaudit.State{Primary: email.PrimaryKind, Verified: flag(u.EmailVerified)},
		&teamaudit.State{Primary: email.PrimaryKind, Verified: flag(true)},
		func() error { return s.o.Accounts.UpdateAccount(ctx, id, change) })
}

// SetPrimary makes the School or the Personal e-mail the Primary e-mail.
// The School e-mail is proven by the YTÜ link; a Personal e-mail must be
// proven first.
func (s *UserAccounts) SetPrimary(ctx context.Context, p authz.Principal, id uuid.UUID, kind string) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserEmailPrimary)
	if err != nil {
		return err
	}
	if kind != PrimarySchool && kind != PrimaryPersonal {
		return ErrInvalid
	}
	email := accountEmail(u)
	address := email.School
	if kind == PrimaryPersonal {
		address = email.Personal
		if address != "" && !email.PersonalVerified {
			return accountError(409, AccountEmailUnverified)
		}
	}
	if address == "" {
		return accountError(409, AccountNoAddress)
	}
	if email.PrimaryKind == kind && u.EmailVerified {
		return nil
	}
	return s.write(ctx, id, teamaudit.ActionUserEmailPrimary,
		&teamaudit.State{Primary: email.PrimaryKind}, &teamaudit.State{Primary: kind},
		func() error {
			return s.o.Accounts.UpdateAccount(ctx, id, AccountUpdate{Email: &address, EmailVerified: flag(true)})
		})
}

// SetRequiredActions sets the panel's required actions to exactly actions;
// any other action the account carries stays.
func (s *UserAccounts) SetRequiredActions(ctx context.Context, p authz.Principal, id uuid.UUID, actions []string) error {
	u, err := s.target(ctx, p, id, teamaudit.ActionUserRequiredActions)
	if err != nil {
		return err
	}
	for _, action := range actions {
		if !slices.Contains(RequiredActionAliases, action) {
			return accountError(422, AccountUnknownAction)
		}
	}
	next := make([]string, 0, len(u.RequiredActions)+len(actions))
	for _, held := range u.RequiredActions {
		if !slices.Contains(RequiredActionAliases, held) {
			next = append(next, held)
		}
	}
	for _, action := range actions {
		if !slices.Contains(next, action) {
			next = append(next, action)
		}
	}
	if sameSet(u.RequiredActions, next) {
		return nil
	}
	return s.write(ctx, id, teamaudit.ActionUserRequiredActions,
		&teamaudit.State{RequiredActions: u.RequiredActions}, &teamaudit.State{RequiredActions: next},
		func() error { return s.o.Accounts.UpdateAccount(ctx, id, AccountUpdate{RequiredActions: &next}) })
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// DeleteCredential removes one of the person's credentials (a password, an
// authenticator app, a passkey).
func (s *UserAccounts) DeleteCredential(ctx context.Context, p authz.Principal, id uuid.UUID, credentialID string) error {
	if _, err := s.target(ctx, p, id, teamaudit.ActionUserCredentialRemove); err != nil {
		return err
	}
	if _, err := uuid.Parse(credentialID); err != nil {
		return accountError(404, AccountNoCredential)
	}
	credentials, err := s.o.Accounts.AccountCredentials(ctx, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(credentials, func(c AccountCredential) bool { return c.ID == credentialID })
	if i < 0 || !slices.Contains(removableCredentialTypes, credentials[i].Type) {
		return accountError(404, AccountNoCredential)
	}
	return s.write(ctx, id, teamaudit.ActionUserCredentialRemove,
		&teamaudit.State{CredentialID: credentialID, CredentialType: credentials[i].Type}, nil,
		func() error { return s.o.Accounts.DeleteCredential(ctx, id, credentialID) })
}

// UnlinkIdentity takes the link to an identity provider off the account.
// Unlinking YTÜ ends the Verified YTÜ account's lock; core's YTÜ-linked
// flag stays (it follows the next YTÜ login).
func (s *UserAccounts) UnlinkIdentity(ctx context.Context, p authz.Principal, id uuid.UUID, provider string) error {
	if _, err := s.target(ctx, p, id, teamaudit.ActionUserIdentityUnlink); err != nil {
		return err
	}
	links, err := s.o.Accounts.LinkedIdentities(ctx, id)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(links, func(l LinkedIdentity) bool { return l.Provider == provider }) {
		return accountError(404, AccountNoProvider)
	}
	return s.write(ctx, id, teamaudit.ActionUserIdentityUnlink, &teamaudit.State{Provider: provider}, nil,
		func() error { return s.o.Accounts.UnlinkIdentity(ctx, id, provider) })
}

// ClearLockout lifts the brute-force protection's temporary lock.
func (s *UserAccounts) ClearLockout(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if _, err := s.target(ctx, p, id, teamaudit.ActionUserLockoutClear); err != nil {
		return err
	}
	return s.write(ctx, id, teamaudit.ActionUserLockoutClear, nil, nil, func() error {
		return s.o.Accounts.ClearLockout(ctx, id)
	})
}

// NameChange is the first and last name an admin writes to Keycloak with
// the club profile (PATCH /v1/users/{id}).
type NameChange struct {
	FirstName *string
	LastName  *string
}

// WriteNames writes the names to the account when they differ from what it
// holds, so that tokens, mail and the Account center show them: users:manage,
// never a service account, and for a name that changes the account's rules
// (not one's own account, a privileged person's only by ADMIN). A person
// core cannot find in Keycloak is left alone. Only the person card's PATCH
// /v1/users/{id} calls it; the Account center's own profile does not.
func (s *UserAccounts) WriteNames(ctx context.Context, p authz.Principal, id uuid.UUID, in NameChange) error {
	if in.FirstName == nil && in.LastName == nil {
		return nil
	}
	if !s.o.Authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
		return ErrForbidden
	}
	// A person who is erased or being erased keeps no name in Keycloak.
	if status, err := s.coreStatus(ctx, id); err != nil || status != user.ReadStatusActive {
		return err
	}
	u, err := s.o.Accounts.AccountUser(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if u.ServiceAccount {
		return accountError(409, AccountServiceAccount)
	}
	change := AccountUpdate{}
	var fields []string
	if in.FirstName != nil && strings.TrimSpace(*in.FirstName) != u.FirstName {
		first := strings.TrimSpace(*in.FirstName)
		change.FirstName = &first
		fields = append(fields, "firstName")
	}
	if in.LastName != nil && strings.TrimSpace(*in.LastName) != u.LastName {
		last := strings.TrimSpace(*in.LastName)
		change.LastName = &last
		fields = append(fields, "lastName")
	}
	if len(fields) == 0 {
		return nil
	}
	// A name that changes follows the account's rules (ADR-0083): not on
	// the caller's own account (the Account center changes it), and a
	// privileged person's only by a caller in the ADMIN root; a refusal is
	// a denied row. A PATCH that leaves the names as Keycloak has them, the
	// club profile of the card included, goes on as before.
	if p.ID == id.String() {
		s.deny(ctx, p, id, teamaudit.ActionUserProfile, AccountOwn)
		return accountError(409, AccountOwn)
	}
	privileged, err := s.privilegedTarget(ctx, id)
	if err != nil {
		return err
	}
	if privileged && !s.callerIsAdmin(p) {
		s.deny(ctx, p, id, teamaudit.ActionUserProfile, AccountPrivilegedTarget)
		return accountError(403, AccountPrivilegedTarget)
	}
	return s.write(ctx, id, teamaudit.ActionUserProfile, nil, &teamaudit.State{Fields: fields},
		func() error { return s.o.Accounts.UpdateAccount(ctx, id, change) })
}
