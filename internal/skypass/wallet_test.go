package skypass

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/googlewallet"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// fakeWalletAPI is Google: it records what core wrote and fails on demand.
type fakeWalletAPI struct {
	mu        sync.Mutex
	classes   []googlewallet.GenericClass
	inserts   []googlewallet.GenericObject
	updates   []googlewallet.GenericObject
	links     [][]googlewallet.ObjectRef
	objects   map[string]bool
	updateErr error
	insertErr error
}

func newFakeWalletAPI() *fakeWalletAPI {
	return &fakeWalletAPI{objects: map[string]bool{}}
}

func (f *fakeWalletAPI) SaveURL(origins []string, objects []googlewallet.ObjectRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links = append(f.links, objects)
	return googlewallet.SaveURLPrefix + "signed." + objects[0].ID, nil
}

func (f *fakeWalletAPI) EnsureGenericClass(_ context.Context, class googlewallet.GenericClass) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.classes = append(f.classes, class)
	return nil
}

func (f *fakeWalletAPI) InsertGenericObject(_ context.Context, object googlewallet.GenericObject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserts = append(f.inserts, object)
	if f.objects[object.ID] {
		return &googlewallet.APIError{Op: "insert object", Status: http.StatusConflict}
	}
	f.objects[object.ID] = true
	return nil
}

func (f *fakeWalletAPI) UpdateGenericObject(_ context.Context, object googlewallet.GenericObject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, object)
	if !f.objects[object.ID] {
		return &googlewallet.APIError{Op: "update object", Status: http.StatusNotFound}
	}
	return nil
}

func (f *fakeWalletAPI) setUpdateErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateErr = err
}

type walletClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *walletClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *walletClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type walletFixture struct {
	t      *testing.T
	users  user.Store
	store  *MemoryWalletStore
	api    *fakeWalletAPI
	clock  *walletClock
	wallet *Wallet
	svc    Service
	key    []byte
}

var testWalletKey = bytes.Repeat([]byte{0x5a}, 32)

func testWalletConfig() GoogleWalletConfig {
	return GoogleWalletConfig{
		Enabled: true, IssuerID: "3388000000022222222", ClassSuffix: "skypass-test",
		TOTPKey: testWalletKey,
	}
}

func newWalletFixture(t *testing.T, config GoogleWalletConfig, api GoogleWalletAPI) *walletFixture {
	t.Helper()
	f := &walletFixture{
		t: t, users: user.NewMemoryStore(), store: NewMemoryWalletStore(),
		clock: &walletClock{now: time.Date(2026, 10, 8, 18, 30, 10, 0, time.UTC)},
		key:   config.TOTPKey,
	}
	if fake, ok := api.(*fakeWalletAPI); ok {
		f.api = fake
	}
	f.wallet = NewWallet(config, api, f.store, f.users, WalletOptions{Now: f.clock.Now})
	f.svc = NewServiceWithWallet(f.users, authz.NewAuthorizer(authz.DefaultPolicy()), NewSigner(testECKey(t), time.Minute), f.wallet)
	return f
}

func (f *walletFixture) member(first, last, skyNumber string) authz.Principal {
	f.t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(f.users).Ensure(context.Background(), id, user.Profile{
		Email: strings.ToLower(first) + "@example.com", FirstName: first, LastName: last, SkyNumber: skyNumber,
	}); err != nil {
		f.t.Fatal(err)
	}
	return authz.Principal{ID: id.String()}
}

// code is what the pass's barcode shows at the given step offset from now.
func (f *walletFixture) code(passID string, steps int) string {
	step := int64(totpCounter(f.clock.Now(), WalletPeriod)) + int64(steps)
	return WalletCodePrefix + passID + ":" + hotp(walletSecret(f.key, passID), uint64(step), WalletDigits)
}

func (f *walletFixture) passID(p authz.Principal) string {
	f.t.Helper()
	pass, ok, err := f.store.ActivePass(context.Background(), uuid.MustParse(p.ID))
	if err != nil || !ok {
		f.t.Fatalf("no active pass: %v", err)
	}
	return pass.PassID
}

var walletStaff = authz.Principal{ID: "99999999-9999-9999-9999-999999999999", Groups: []string{"/UYELER/YK"}}

func TestWalletLinkWritesThePassAndSignsALinkThatOnlyNamesIt(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")

	status, err := f.svc.WalletStatus(ctx, ada)
	if err != nil || !status.Google.Available || status.Google.Issued {
		t.Fatalf("status before %+v %v", status, err)
	}
	link, err := f.svc.GoogleWalletLink(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	objectID := "3388000000022222222.sp-" + passID
	if link.SaveURL != googlewallet.SaveURLPrefix+"signed."+objectID {
		t.Fatalf("link %q", link.SaveURL)
	}
	if len(f.api.classes) != 1 {
		t.Fatalf("classes %+v", f.api.classes)
	}
	class := f.api.classes[0]
	if class.ID != "3388000000022222222.skypass-test" || class.MultipleDevicesAndHoldersAllowedStatus != "ONE_USER_ALL_DEVICES" ||
		class.ViewUnlockRequirement != "UNLOCK_REQUIRED_TO_VIEW" {
		t.Fatalf("class %+v", class)
	}
	if len(f.api.inserts) != 1 {
		t.Fatalf("inserts %d", len(f.api.inserts))
	}
	object := f.api.inserts[0]
	if object.ID != objectID || object.ClassID != class.ID || object.State != "ACTIVE" {
		t.Fatalf("object %+v", object)
	}
	if object.Header.DefaultValue.Value != "Ada Lovelace" || object.CardTitle.DefaultValue.Value != "SKY LAB" {
		t.Fatalf("face %+v %+v", object.Header, object.CardTitle)
	}
	if len(object.TextModulesData) != 1 || object.TextModulesData[0].Body != "SKY-0000042" {
		t.Fatalf("text modules %+v", object.TextModulesData)
	}
	if object.Logo != nil {
		t.Fatalf("logo without a configured one: %+v", object.Logo)
	}
	barcode := object.RotatingBarcode
	if barcode == nil || barcode.Type != "QR_CODE" || barcode.ValuePattern != "SPW1:"+passID+":{totp_value_0}" || barcode.AlternateText != "SKY-0000042" {
		t.Fatalf("barcode %+v", barcode)
	}
	totp := barcode.TotpDetails
	if totp == nil || totp.PeriodMillis != "60000" || totp.Algorithm != "TOTP_SHA1" || len(totp.Parameters) != 1 || totp.Parameters[0].ValueLength != 8 {
		t.Fatalf("totp %+v", totp)
	}
	if totp.Parameters[0].Key != strings.ToUpper(hex.EncodeToString(walletSecret(testWalletKey, passID))) {
		t.Fatal("the object's key is not the pass's secret")
	}
	if object.PassConstraints == nil || object.PassConstraints.ScreenshotEligibility != "INELIGIBLE" {
		t.Fatalf("pass constraints %+v", object.PassConstraints)
	}
	encoded, _ := json.Marshal(object)
	for _, never := range []string{"picture", "Picture", "image", "Image", "@example.com"} {
		if strings.Contains(string(encoded), never) {
			t.Fatalf("the pass carries %q: %s", never, encoded)
		}
	}
	if len(f.api.links) != 1 || len(f.api.links[0]) != 1 || f.api.links[0][0] != (googlewallet.ObjectRef{ID: objectID, ClassID: class.ID}) {
		t.Fatalf("link refs %+v", f.api.links)
	}

	// Asked again (another device, or the name changed): the same pass is
	// rewritten, the class is not.
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if f.passID(ada) != passID || len(f.api.classes) != 1 || len(f.api.updates) != 1 || f.api.updates[0].ID != objectID {
		t.Fatalf("second link: pass %s classes %d updates %+v", f.passID(ada), len(f.api.classes), f.api.updates)
	}
	status, err = f.svc.WalletStatus(ctx, ada)
	if err != nil || !status.Google.Available || !status.Google.Issued {
		t.Fatalf("status after %+v %v", status, err)
	}
}

func TestWalletPassWithoutSkyNumberHidesTheCodeText(t *testing.T) {
	t.Parallel()
	config := testWalletConfig()
	config.LogoURL = "https://cdn.yildizskylab.com/logo.png"
	f := newWalletFixture(t, config, newFakeWalletAPI())
	object := f.wallet.passObject(WalletPass{PassID: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"}, user.User{FirstName: " Grace ", LastName: "Hopper"})
	if object.Header.DefaultValue.Value != "Grace Hopper" {
		t.Fatalf("name %q", object.Header.DefaultValue.Value)
	}
	if object.RotatingBarcode.AlternateText != "SkyPass" || len(object.TextModulesData) != 0 {
		t.Fatalf("object %+v", object)
	}
	if object.Logo == nil || object.Logo.SourceURI.URI != "https://cdn.yildizskylab.com/logo.png" {
		t.Fatalf("logo %+v", object.Logo)
	}
}

func TestWalletOffIssuesNothingAndReadsNoCode(t *testing.T) {
	t.Parallel()
	for name, wallet := range map[string]func(*walletFixture) Service{
		"no wallet": func(f *walletFixture) Service {
			return NewService(f.users, authz.NewAuthorizer(authz.DefaultPolicy()), NewSigner(testECKey(t), time.Minute))
		},
		"off": func(f *walletFixture) Service { return f.svc },
	} {
		f := newWalletFixture(t, GoogleWalletConfig{}, nil)
		svc := wallet(f)
		ctx := context.Background()
		ada := f.member("Ada", "Lovelace", "SKY-0000042")
		status, err := svc.WalletStatus(ctx, ada)
		if err != nil || status.Google.Available || status.Google.Issued {
			t.Fatalf("%s: status %+v %v", name, status, err)
		}
		if _, err := svc.GoogleWalletLink(ctx, ada); !errors.Is(err, ErrWalletOff) {
			t.Fatalf("%s: link %v", name, err)
		}
		if err := svc.RevokeGoogleWallet(ctx, ada); !errors.Is(err, ErrWalletOff) {
			t.Fatalf("%s: revoke %v", name, err)
		}
		// A code from a pass issued while it was on is not read.
		passID := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		if _, err := f.store.CreatePass(ctx, uuid.MustParse(ada.ID), passID, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
		code := WalletCodePrefix + passID + ":" + hotp(walletSecret(testWalletKey, passID), totpCounter(f.clock.Now(), WalletPeriod), WalletDigits)
		if _, err := svc.Verify(ctx, walletStaff, code); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: verify %v", name, err)
		}
		if _, err := svc.HolderFrom(ctx, walletStaff, code, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: settle %v", name, err)
		}
	}
}

func TestWalletCodeIsTakenOneStepEitherSideOfNow(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	for _, steps := range []int{-1, 0, 1} {
		got, err := f.svc.Verify(ctx, walletStaff, f.code(passID, steps))
		if err != nil || got.ID.String() != ada.ID || got.SkyNumber != "SKY-0000042" || got.FirstName != "Ada" {
			t.Fatalf("step %+d: %+v %v", steps, got, err)
		}
	}
	for _, steps := range []int{-3, -2, 2, 3} {
		if _, err := f.svc.Verify(ctx, walletStaff, f.code(passID, steps)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("step %+d accepted: %v", steps, err)
		}
	}
	if _, err := f.svc.Verify(ctx, walletStaff, "SPW1:"+passID+":00000000"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong digits %v", err)
	}
	// Verify is still staff-only.
	if _, err := f.svc.Verify(ctx, ada, f.code(passID, 0)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member verify %v", err)
	}
}

func TestWalletCodeChecksInOnce(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	now := f.code(passID, 0)

	// A look at who this is spends nothing.
	for range 2 {
		if _, err := f.svc.Verify(ctx, walletStaff, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.svc.HolderFrom(ctx, walletStaff, now, "")
	if err != nil || got.ID.String() != ada.ID {
		t.Fatalf("settle %+v %v", got, err)
	}
	// The same code again (a screenshot, a second door, a retry), or the
	// code before it, checks no one in.
	if _, err := f.svc.HolderFrom(ctx, walletStaff, now, ""); !errors.Is(err, ErrWalletCodeUsed) {
		t.Fatalf("replay %v", err)
	}
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(passID, -1), ""); !errors.Is(err, ErrWalletCodeUsed) {
		t.Fatalf("older code %v", err)
	}
	if _, err := f.svc.Verify(ctx, walletStaff, now); !errors.Is(err, ErrWalletCodeUsed) {
		t.Fatalf("verify of a spent code %v", err)
	}
	// The pass's next code works.
	f.clock.advance(WalletPeriod)
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(passID, 0), ""); err != nil {
		t.Fatalf("next code %v", err)
	}
	if strings.Contains(f.wallet.Prometheus(), passID) {
		t.Fatal("metrics name a pass")
	}
	if !strings.Contains(f.wallet.Prometheus(), `skylab_skypass_wallet_codes_total{outcome="used"} 3`) {
		t.Fatalf("metrics %s", f.wallet.Prometheus())
	}
}

func TestWalletCodesWaitAfterTooManyWrongOnes(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	grace := f.member("Grace", "Hopper", "SKY-0000043")
	for _, p := range []authz.Principal{ada, grace} {
		if _, err := f.svc.GoogleWalletLink(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	adaPass, gracePass := f.passID(ada), f.passID(grace)
	wrong := WalletCodePrefix + adaPass + ":" + "12345678"
	if wrong == f.code(adaPass, 0) || wrong == f.code(adaPass, -1) || wrong == f.code(adaPass, 1) {
		t.Skip("the fixed wrong code happens to be right")
	}
	for i := range DefaultWalletFailureLimit {
		if _, err := f.svc.HolderFrom(ctx, walletStaff, wrong, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	_, err := f.svc.HolderFrom(ctx, walletStaff, f.code(adaPass, 0), "")
	var limited *WalletRateLimitError
	if !errors.As(err, &limited) || !errors.Is(err, ErrWalletRateLimited) || limited.RetryAfter <= 0 || limited.RetryAfter > DefaultWalletFailureWindow {
		t.Fatalf("right code while limited: %v", err)
	}
	// Another person's pass is not held up, nor is another scanner: a
	// stranger who saw Ada's pass id cannot lock her out of the door.
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(gracePass, 0), ""); err != nil {
		t.Fatalf("other pass %v", err)
	}
	otherScanner := authz.Principal{ID: "88888888-8888-8888-8888-888888888888", Groups: []string{"/UYELER/YK"}}
	if _, err := f.svc.Verify(ctx, otherScanner, f.code(adaPass, 0)); err != nil {
		t.Fatalf("other scanner %v", err)
	}
	f.clock.advance(DefaultWalletFailureWindow)
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(adaPass, 0), ""); err != nil {
		t.Fatalf("after the window %v", err)
	}
}

func TestWalletCodeOfAPersonBeingErasedIsRefused(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	if _, err := f.users.RequestDeletion(ctx, uuid.MustParse(ada.ID), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(passID, 0), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settle %v", err)
	}
	if _, err := f.svc.GoogleWalletLink(ctx, ada); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link %v", err)
	}
}

func TestWalletRevokeStopsTheCodesAndEmptiesGooglesCopy(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	code := f.code(passID, 0)
	if err := f.svc.RevokeGoogleWallet(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.HolderFrom(ctx, walletStaff, code, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("code of a revoked pass %v", err)
	}
	if len(f.api.updates) != 1 {
		t.Fatalf("updates %+v", f.api.updates)
	}
	withdrawn := f.api.updates[0]
	if withdrawn.ID != "3388000000022222222.sp-"+passID || withdrawn.State != "INACTIVE" || withdrawn.RotatingBarcode != nil || len(withdrawn.TextModulesData) != 0 {
		t.Fatalf("withdrawn %+v", withdrawn)
	}
	encoded, _ := json.Marshal(withdrawn)
	if strings.Contains(string(encoded), "Ada") || strings.Contains(string(encoded), "SKY-0000042") {
		t.Fatalf("Google keeps the person: %s", encoded)
	}
	if passes, _ := f.store.PassesOf(ctx, uuid.MustParse(ada.ID)); len(passes) != 0 {
		t.Fatalf("rows left %+v", passes)
	}
	status, _ := f.svc.WalletStatus(ctx, ada)
	if status.Google.Issued {
		t.Fatal("still issued")
	}
	// Revoking with nothing issued is fine.
	if err := f.svc.RevokeGoogleWallet(ctx, ada); err != nil {
		t.Fatal(err)
	}
	// The next link is a new pass with a new secret.
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if f.passID(ada) == passID {
		t.Fatal("the pass came back")
	}
}

func TestWalletRevokeWhileGoogleIsDownStillStopsTheCodes(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	oldPass := f.passID(ada)
	f.api.setUpdateErr(&googlewallet.APIError{Op: "update object", Status: http.StatusServiceUnavailable})
	if err := f.svc.RevokeGoogleWallet(ctx, ada); !errors.Is(err, ErrWalletUpstream) {
		t.Fatalf("revoke %v", err)
	}
	if _, err := f.svc.HolderFrom(ctx, walletStaff, f.code(oldPass, 0), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("code of the revoked pass %v", err)
	}
	// Google is back: the next link withdraws the old pass first.
	f.api.setUpdateErr(nil)
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	if f.passID(ada) == oldPass {
		t.Fatal("the revoked pass came back")
	}
	if len(f.api.updates) != 1 || f.api.updates[0].ID != "3388000000022222222.sp-"+oldPass || f.api.updates[0].State != "INACTIVE" {
		t.Fatalf("updates %+v", f.api.updates)
	}
	if _, ok, _ := f.store.PassByID(ctx, oldPass); ok {
		t.Fatal("the old row is left")
	}
}

func TestWalletEraseSubjectWithdrawsEveryPass(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, testWalletConfig(), newFakeWalletAPI())
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	adaID := uuid.MustParse(ada.ID)

	// Nobody issued: nothing to do.
	if n, err := f.wallet.EraseSubject(ctx, adaID); n != 0 || err != nil {
		t.Fatalf("erase without passes %d %v", n, err)
	}
	if _, err := f.svc.GoogleWalletLink(ctx, ada); err != nil {
		t.Fatal(err)
	}
	passID := f.passID(ada)
	code := f.code(passID, 0)

	f.api.setUpdateErr(&googlewallet.APIError{Op: "update object", Status: http.StatusInternalServerError})
	_, err := f.wallet.EraseSubject(ctx, adaID)
	var later interface{ RetryAt() time.Time }
	if !errors.Is(err, ErrWalletUpstream) || !errors.As(err, &later) || !later.RetryAt().After(f.clock.Now()) {
		// Google being down defers the erasure like a service that is
		// down: it does not spend the request's attempts.
		t.Fatalf("erase while Google is down %v", err)
	}
	// A refusal is not waited out: it needs a person.
	f.api.setUpdateErr(&googlewallet.APIError{Op: "update object", Status: http.StatusForbidden, Reason: "PERMISSION_DENIED"})
	if _, err := f.wallet.EraseSubject(ctx, adaID); !errors.Is(err, ErrWalletUpstream) || errors.As(err, &later) {
		t.Fatalf("erase refused by Google %v", err)
	}
	if strings.Contains(errorText(f.wallet.EraseSubject(ctx, adaID)), passID) {
		t.Fatal("the error names the pass")
	}
	// The code stopped at the first try.
	if _, err := f.svc.HolderFrom(ctx, walletStaff, code, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("code during erasure %v", err)
	}
	f.api.setUpdateErr(nil)
	if n, err := f.wallet.EraseSubject(ctx, adaID); n != 1 || err != nil {
		t.Fatalf("erase %d %v", n, err)
	}
	if passes, _ := f.store.PassesOf(ctx, adaID); len(passes) != 0 {
		t.Fatalf("rows left %+v", passes)
	}
	if n, err := f.wallet.EraseSubject(ctx, adaID); n != 0 || err != nil {
		t.Fatalf("erase again %d %v", n, err)
	}
}

func TestWalletEraseSubjectWithGoogleWalletOffFailsWhilePassesAreLeft(t *testing.T) {
	t.Parallel()
	f := newWalletFixture(t, GoogleWalletConfig{}, nil)
	ctx := context.Background()
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	adaID := uuid.MustParse(ada.ID)
	if n, err := f.wallet.EraseSubject(ctx, adaID); n != 0 || err != nil {
		t.Fatalf("off, nothing issued: %d %v", n, err)
	}
	// A pass from when it was on.
	if _, err := f.store.CreatePass(ctx, adaID, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.wallet.EraseSubject(ctx, adaID); !errors.Is(err, ErrWalletOff) {
		t.Fatalf("off with a pass left: %v", err)
	}
	pass, _, _ := f.store.PassByID(ctx, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if pass.RevokedAt == nil {
		t.Fatal("the pass still opens the door")
	}
	var nilWallet *Wallet
	if _, err := nilWallet.EraseSubject(ctx, adaID); err == nil {
		t.Fatal("a missing wallet store passed as erased")
	}
}

func errorText(_ int64, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestWalletLinkFailsCleanlyWhenGoogleRefuses(t *testing.T) {
	t.Parallel()
	api := newFakeWalletAPI()
	api.insertErr = &googlewallet.APIError{Op: "insert object", Status: http.StatusForbidden, Reason: "PERMISSION_DENIED"}
	var logged []string
	f := newWalletFixture(t, testWalletConfig(), api)
	f.wallet.logf = func(format string, args ...any) {
		logged = append(logged, format)
	}
	ada := f.member("Ada", "Lovelace", "SKY-0000042")
	if _, err := f.svc.GoogleWalletLink(context.Background(), ada); !errors.Is(err, ErrWalletUpstream) {
		t.Fatalf("link %v", err)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %v", logged)
	}
	if !strings.Contains(f.wallet.Prometheus(), `skylab_skypass_wallet_links_total{outcome="failed"} 1`) {
		t.Fatalf("metrics %s", f.wallet.Prometheus())
	}
}

// walletEnv is a full, valid configuration.
func walletEnv(t *testing.T) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	account, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "wallet@skylab.iam.gserviceaccount.com", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   "https://oauth2.googleapis.com/token",
	})
	return map[string]string{
		GoogleWalletEnabledEnv:        "true",
		GoogleWalletIssuerIDEnv:       "3388000000022222222",
		GoogleWalletClassSuffixEnv:    "skypass-production",
		GoogleWalletServiceAccountEnv: base64.StdEncoding.EncodeToString(account),
		GoogleWalletTOTPKeyEnv:        base64.RawURLEncoding.EncodeToString(testWalletKey),
		GoogleWalletOriginsEnv:        "https://hesap.yildizskylab.com, https://yildizskylab.com",
		GoogleWalletLogoURLEnv:        "https://cdn.yildizskylab.com/skypass/logo.png",
	}
}

func TestGoogleWalletConfigFromEnv(t *testing.T) {
	t.Parallel()
	off, err := GoogleWalletConfigFromEnv(func(string) string { return "" })
	if err != nil || off.Enabled {
		t.Fatalf("unset %+v %v", off, err)
	}
	env := walletEnv(t)
	config, err := GoogleWalletConfigFromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.ClassID() != "3388000000022222222.skypass-production" || len(config.TOTPKey) != 32 ||
		config.Account.ClientEmail != "wallet@skylab.iam.gserviceaccount.com" || len(config.Origins) != 2 ||
		config.LogoURL != "https://cdn.yildizskylab.com/skypass/logo.png" {
		t.Fatalf("config %+v", config)
	}

	// Off reads nothing else, even values that would not parse.
	broken := map[string]string{GoogleWalletEnabledEnv: "false", GoogleWalletTOTPKeyEnv: "short"}
	if config, err := GoogleWalletConfigFromEnv(func(name string) string { return broken[name] }); err != nil || config.Enabled {
		t.Fatalf("false %+v %v", config, err)
	}

	cases := map[string]func(map[string]string){
		"enabled typo":   func(m map[string]string) { m[GoogleWalletEnabledEnv] = "yes" },
		"no issuer":      func(m map[string]string) { delete(m, GoogleWalletIssuerIDEnv) },
		"issuer letters": func(m map[string]string) { m[GoogleWalletIssuerIDEnv] = "issuer-x" },
		"no class":       func(m map[string]string) { delete(m, GoogleWalletClassSuffixEnv) },
		"class slash":    func(m map[string]string) { m[GoogleWalletClassSuffixEnv] = "sky/pass" },
		"no account":     func(m map[string]string) { delete(m, GoogleWalletServiceAccountEnv) },
		"bad account":    func(m map[string]string) { m[GoogleWalletServiceAccountEnv] = "c2VjcmV0LXZhbHVl" },
		"no key":         func(m map[string]string) { delete(m, GoogleWalletTOTPKeyEnv) },
		"short key": func(m map[string]string) {
			m[GoogleWalletTOTPKeyEnv] = base64.RawURLEncoding.EncodeToString([]byte("secret-value"))
		},
		"padded key": func(m map[string]string) {
			m[GoogleWalletTOTPKeyEnv] = base64.URLEncoding.EncodeToString(testWalletKey)
		},
		"origin path":     func(m map[string]string) { m[GoogleWalletOriginsEnv] = "https://yildizskylab.com/wallet" },
		"origin http":     func(m map[string]string) { m[GoogleWalletOriginsEnv] = "http://yildizskylab.com" },
		"logo over http":  func(m map[string]string) { m[GoogleWalletLogoURLEnv] = "http://cdn.yildizskylab.com/logo.png" },
		"logo not an URL": func(m map[string]string) { m[GoogleWalletLogoURLEnv] = "logo.png" },
	}
	for name, change := range cases {
		env := walletEnv(t)
		change(env)
		_, err := GoogleWalletConfigFromEnv(func(name string) string { return env[name] })
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		for _, value := range []string{"secret-value", "c2VjcmV0LXZhbHVl", env[GoogleWalletTOTPKeyEnv]} {
			if value != "" && strings.Contains(err.Error(), value) {
				t.Fatalf("%s: the error carries a value: %v", name, err)
			}
		}
	}
	// A local dev origin may be http.
	env = walletEnv(t)
	env[GoogleWalletOriginsEnv] = "http://localhost:3000"
	if _, err := GoogleWalletConfigFromEnv(func(name string) string { return env[name] }); err != nil {
		t.Fatalf("localhost origin %v", err)
	}
}
