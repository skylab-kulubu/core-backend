package media_test

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
)

// reviewer is the Skyforms reviewer the service account acts for.
var reviewer = uuid.MustParse("80808080-8080-8080-8080-808080808080")

// tokenOf is the token query parameter of a read link.
func tokenOf(t *testing.T, link media.ReadLink) string {
	t.Helper()
	parsed, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("token")
}

func readAll(t *testing.T, content media.Content) []byte {
	t.Helper()
	defer content.Body.Close()
	data, err := io.ReadAll(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestService_OwningProductGetsAFiveMinuteReadLink(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("81818181-8181-8181-8181-818181818181"))
	issuedAt := pm.bao.Clock.Now()

	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "https://api.example.test/v1/media/" + file.ID.String() + "/content?token="
	if !strings.HasPrefix(link.URL, wantPrefix) || tokenOf(t, link) == "" {
		t.Fatalf("url %q", link.URL)
	}
	if !link.ExpiresAt.Equal(issuedAt.Add(5 * time.Minute)) {
		t.Fatalf("expires at %s, issued at %s", link.ExpiresAt, issuedAt)
	}

	log := pm.store.ReadLinkLog(file.ID)
	if len(log) != 1 {
		t.Fatalf("%d access log rows", len(log))
	}
	row := log[0]
	if row.MediaID != file.ID || row.Product != authz.ProductForms || row.OnBehalfOf != reviewer ||
		!row.IssuedAt.Equal(issuedAt) || !row.ExpiresAt.Equal(link.ExpiresAt) || len(row.Opens) != 0 {
		t.Fatalf("access log row %+v", row)
	}
}

func TestService_ReadLinkIsOnlyForTheOwningProduct(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	respondent := signedIn("82828282-8282-8282-8282-828282828282")
	file := pm.answerFile(t, respondent)
	picture, err := pm.svc.UploadForPurpose(context.Background(), respondent, "profile_picture", uploaded("me.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	withoutRole := formsService
	withoutRole.Roles = nil

	for name, tc := range map[string]struct {
		p          authz.Principal
		id         uuid.UUID
		onBehalfOf string
		want       error
	}{
		"the uploader":                  {respondent, file.ID, reviewer.String(), media.ErrLinkForbidden},
		"a member":                      {signedIn("82828282-0000-8282-8282-828282828282"), file.ID, "", media.ErrLinkForbidden},
		"an admin, for a Skyforms file": {admin(), file.ID, "", media.ErrNotFound},
		"Skyforms without media:attach": {withoutRole, file.ID, reviewer.String(), media.ErrLinkForbidden},
		"another product":               {cmsService, file.ID, reviewer.String(), media.ErrNotFound},
		"a public Media":                {formsService, picture.ID, reviewer.String(), media.ErrNotFound},
		"a Media that does not exist":   {formsService, uuid.New(), reviewer.String(), media.ErrNotFound},
		"no acting person":              {formsService, file.ID, "", media.ErrInvalid},
		"an acting person not a UUID":   {formsService, file.ID, "reviewer", media.ErrInvalid},
		"a person with a malformed id":  {withoutRole, file.ID, "reviewer", media.ErrLinkForbidden},
	} {
		if _, err := pm.svc.IssueReadLink(context.Background(), tc.p, tc.id, tc.onBehalfOf); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if log := pm.store.ReadLinkLog(file.ID); len(log) != 0 {
		t.Fatalf("refused links left %d access log rows", len(log))
	}
}

func TestService_ReadLinkOpensTheDecryptedFileAndLogsTheOpen(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("83838383-8383-8383-8383-838383838383"))
	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}
	pm.bao.Clock.Advance(2 * time.Minute)

	content, err := pm.svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, content); !bytes.Equal(got, pdfFile()) {
		t.Fatalf("opened %q", got)
	}
	if content.Name != "cv.pdf" || content.Type != "application/pdf" || content.Size != int64(len(pdfFile())) {
		t.Fatalf("content %+v", content)
	}
	log := pm.store.ReadLinkLog(file.ID)
	if len(log) != 1 || len(log[0].Opens) != 1 {
		t.Fatalf("access log %+v", log)
	}
	if open := log[0].Opens[0]; open.ClientIP != "203.0.113.9" || !open.OpenedAt.Equal(pm.bao.Clock.Now()) {
		t.Fatalf("open %+v", open)
	}
}

func TestService_ExpiredReadLinkIsRefused(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("84848484-8484-8484-8484-848484848484"))
	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}

	pm.bao.Clock.Advance(5 * time.Minute)
	if _, err := pm.svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrLinkExpired) {
		t.Fatalf("err = %v, want %v", err, media.ErrLinkExpired)
	}
	if log := pm.store.ReadLinkLog(file.ID); len(log[0].Opens) != 0 {
		t.Fatal("a refused open was logged")
	}
}

func TestService_TamperedReadLinkIsRefused(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	respondent := signedIn("85858585-8585-8585-8585-858585858585")
	file, other := pm.answerFile(t, respondent), pm.answerFile(t, respondent)
	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}
	token := tokenOf(t, link)
	flipped := []byte(token)
	flipped[10] ^= 0x01

	for name, tc := range map[string]struct {
		id    uuid.UUID
		token string
	}{
		"a changed token":           {file.ID, string(flipped)},
		"another Media's id":        {other.ID, token},
		"no token":                  {file.ID, ""},
		"not a token":               {file.ID, "not-a-token"},
		"the signature of another":  {file.ID, token[:strings.Index(token, ".")] + ".AAAA"},
		"a token with no signature": {file.ID, token[:strings.Index(token, ".")]},
	} {
		if _, err := pm.svc.OpenContent(context.Background(), tc.id, tc.token, "203.0.113.9"); !errors.Is(err, media.ErrLinkInvalid) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrLinkInvalid)
		}
	}
}

func TestService_ReadLinksStillOpenFilesAfterKeyRotation(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	respondent := signedIn("86868686-8686-8686-8686-868686868686")
	before := pm.answerFile(t, respondent)
	pm.bao.Rotate()
	after := pm.answerFile(t, respondent)

	for _, file := range []media.Media{before, after} {
		link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
		if err != nil {
			t.Fatal(err)
		}
		content, err := pm.svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9")
		if err != nil {
			t.Fatalf("%s: %v", file.ID, err)
		}
		if got := readAll(t, content); !bytes.Equal(got, pdfFile()) {
			t.Fatalf("%s: opened %q", file.ID, got)
		}
	}
	if record, _ := pm.store.Get(context.Background(), after.ID); record.Encryption.KeyVersion != 2 {
		t.Fatalf("after rotation the key version is %d", record.Encryption.KeyVersion)
	}
}

func TestService_OpenBaoDownRefusesPrivateReads(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("87878787-8787-8787-8787-878787878787"))
	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}

	pm.bao.Seal()
	if _, err := pm.svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrPrivateUnavailable) {
		t.Fatalf("err = %v, want %v", err, media.ErrPrivateUnavailable)
	}
	if log := pm.store.ReadLinkLog(file.ID); len(log[0].Opens) != 0 {
		t.Fatal("a failed open was logged")
	}
}

func TestService_ChangedCiphertextIsNeverServed(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("88888888-8888-8888-8888-888888888888"))
	stored, _ := pm.private.Get(file.Key)
	changed := bytes.Clone(stored)
	changed[len(changed)-20] ^= 0x01
	if err := pm.private.Put(context.Background(), file.Key, changed, media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	link, err := pm.svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pm.svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrPrivateIntegrity) {
		t.Fatalf("err = %v, want %v", err, media.ErrPrivateIntegrity)
	}
}

func TestService_WrongKeyVersionIsRefused(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	file := pm.answerFile(t, signedIn("89898989-8989-8989-8989-898989898989"))
	record, err := pm.store.Get(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrappedBody := strings.TrimPrefix(record.Encryption.WrappedKey, "vault:v1:")

	for name, enc := range map[string]media.Encryption{
		"recorded version differs from the wrapped key": {Algorithm: record.Encryption.Algorithm, WrappedKey: record.Encryption.WrappedKey, KeyVersion: 2},
		"a version the Transit key does not have":       {Algorithm: record.Encryption.Algorithm, WrappedKey: "vault:v3:" + wrappedBody, KeyVersion: 3},
	} {
		copied := record
		copied.ID = uuid.New()
		copied.Encryption = &enc
		if _, err := pm.store.Create(ctx, copied); err != nil {
			t.Fatal(err)
		}
		link, err := pm.svc.IssueReadLink(ctx, formsService, copied.ID, reviewer.String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pm.svc.OpenContent(ctx, copied.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrPrivateIntegrity) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrPrivateIntegrity)
		}
	}
}

// A privileged admin opens core's own private Media (a certificate asset in
// the template editor) through a link issued to them.
func TestService_AdminGetsAReadLinkToCoresOwnPrivateMedia(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	editor := admin()
	asset, err := pm.svc.UploadForPurpose(context.Background(), editor, "certificate_asset", uploaded("background.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}

	link, err := pm.svc.IssueReadLink(context.Background(), editor, asset.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	content, err := pm.svc.OpenContent(context.Background(), asset.ID, tokenOf(t, link), "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	// The 1-pixel PNG as it was stored: re-encoded, still 1 by 1.
	if got, err := png.DecodeConfig(bytes.NewReader(readAll(t, content))); err != nil || got.Width != 1 || got.Height != 1 {
		t.Fatalf("opened %+v, %v", got, err)
	}
	actor := uuid.MustParse(editor.ID)
	if log := pm.store.ReadLinkLog(asset.ID); len(log) != 1 || log[0].Product != authz.ProductCore || log[0].OnBehalfOf != actor {
		t.Fatalf("access log %+v", log)
	}

	for name, tc := range map[string]struct {
		p          authz.Principal
		onBehalfOf string
		want       error
	}{
		"an admin naming someone else": {editor, reviewer.String(), media.ErrInvalid},
		"an admin naming no UUID":      {editor, "not-a-uuid", media.ErrInvalid},
		"a team leader":                {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, "", media.ErrLinkForbidden},
		"Skyforms":                     {formsService, reviewer.String(), media.ErrNotFound},
	} {
		if _, err := pm.svc.IssueReadLink(context.Background(), tc.p, asset.ID, tc.onBehalfOf); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// The ciphertext is bound to its object key: the bytes of one private object
// copied under another key do not open.
func TestService_PrivateObjectCopiedToAnotherKeyIsNeverServed(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	file := pm.answerFile(t, signedIn("8b8b8b8b-8b8b-8b8b-8b8b-8b8b8b8b8b8b"))
	record, err := pm.store.Get(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := pm.private.Get(record.Key)
	copied := record
	copied.ID, copied.Key = uuid.New(), media.PrivateObjectKey("files/"+uuid.NewString())
	if err := pm.private.Put(ctx, copied.Key, stored, media.BlobMetadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pm.store.Create(ctx, copied); err != nil {
		t.Fatal(err)
	}
	link, err := pm.svc.IssueReadLink(ctx, formsService, copied.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pm.svc.OpenContent(ctx, copied.ID, tokenOf(t, link), "203.0.113.9"); !errors.Is(err, media.ErrPrivateIntegrity) {
		t.Fatalf("err = %v, want %v", err, media.ErrPrivateIntegrity)
	}
}

func TestService_ReadLinksAreRefusedWhilePrivateMediaIsOff(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	if _, err := svc.IssueReadLink(context.Background(), formsService, uuid.New(), reviewer.String()); !errors.Is(err, media.ErrPrivateMediaDisabled) {
		t.Fatalf("link: err = %v", err)
	}
	if _, err := svc.OpenContent(context.Background(), uuid.New(), "token", "203.0.113.9"); !errors.Is(err, media.ErrPrivateMediaDisabled) {
		t.Fatalf("open: err = %v", err)
	}
}

// An OpenBao without the Transit key core is configured with is an outage
// to fix, not a file that failed its check.
func TestService_MissingTransitKeyIsUnavailableNotIntegrity(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	file := pm.answerFile(t, signedIn("8c8c8c8c-8c8c-8c8c-8c8c-8c8c8c8c8c8c"))
	config := pm.bao.Config()
	config.Key = "missing"
	svc := media.NewServiceWithOptions(pm.store, pm.public, authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: media.NewPrivateStorage(pm.private, transit.New(config)), LinkKey: bytes.Repeat([]byte{7}, 32),
				LinkOrigin: "https://api.example.test", AccessLog: pm.store, Now: pm.bao.Clock.Now,
			},
		})
	link, err := svc.IssueReadLink(context.Background(), formsService, file.ID, reviewer.String())
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.OpenContent(context.Background(), file.ID, tokenOf(t, link), "203.0.113.9")
	if !errors.Is(err, media.ErrPrivateUnavailable) || errors.Is(err, media.ErrPrivateIntegrity) {
		t.Fatalf("err = %v, want %v", err, media.ErrPrivateUnavailable)
	}
	if !strings.Contains(err.Error(), "transit/test") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("the error does not name the mount and key: %v", err)
	}
}
