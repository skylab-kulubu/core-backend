package media_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/config"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/envelope"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// privateMedia is a media service with private Media on: a fake OpenBao
// and a private bucket next to the public one.
type privateMedia struct {
	svc     media.Service
	store   *media.MemoryStore
	public  *media.MemoryBlob
	private *media.MemoryBlob
	storage *media.PrivateStorage
	bao     *transittest.Server
}

// unscannedCatalogue is the reviewed catalogue with answer_file's malware
// scan lifted. No scanner exists until ticket 12, so the reviewed answer_file
// cannot be uploaded at all; these tests store Answer files as they will be
// stored once a scanner has passed them.
func unscannedCatalogue(t testing.TB) media.Catalogue {
	t.Helper()
	var file map[string]any
	if err := json.Unmarshal(config.MediaPurposes, &file); err != nil {
		t.Fatal(err)
	}
	file["purposes"].(map[string]any)["answer_file"].(map[string]any)["scan"] = false
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := media.ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func newPrivateMedia(t *testing.T) privateMedia {
	t.Helper()
	bao := transittest.NewServer(t)
	store := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	svc := media.NewServiceWithOptions(store, public, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Catalogue:       unscannedCatalogue(t),
			Private: &media.PrivateMedia{
				Storage: storage, LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test/",
				AccessLog: store, Now: bao.Clock.Now,
			},
		})
	return privateMedia{svc: svc, store: store, public: public, private: private, storage: storage, bao: bao}
}

func pdfFile() []byte {
	return []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n%%EOF\n")
}

func TestService_AnswerFileIsStoredEncryptedInThePrivateBucket(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()

	created, err := pm.svc.UploadForPurpose(ctx, signedIn("60606060-6060-6060-6060-606060606060"), "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if err != nil {
		t.Fatal(err)
	}
	if created.Visibility != media.VisibilityPrivate || created.URL != "" || created.Purpose != "answer_file" {
		t.Fatalf("created %+v", created)
	}
	if _, ok := pm.public.Get(created.Key); ok {
		t.Fatal("the answer file reached the public bucket")
	}
	stored, ok := pm.private.Get(created.Key)
	if !ok || bytes.Contains(stored, pdfFile()) {
		t.Fatalf("private object present %v, holds the plaintext %v", ok, ok && bytes.Contains(stored, pdfFile()))
	}

	record, err := pm.store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	enc := record.Encryption
	if enc == nil || enc.Algorithm != envelope.Algorithm || enc.KeyVersion != 1 || enc.WrappedKey[:9] != "vault:v1:" {
		t.Fatalf("encryption %+v", enc)
	}
	sealed, _ := record.Sealed()
	plaintext, err := pm.storage.Read(ctx, sealed)
	if err != nil || !bytes.Equal(plaintext, pdfFile()) {
		t.Fatalf("read back %q, %v", plaintext, err)
	}
}

func TestService_CertificateAssetIsStoredPrivate(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := pm.svc.UploadForPurpose(context.Background(), leader, "certificate_asset", uploaded("background.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	if created.Visibility != media.VisibilityPrivate || created.URL != "" || len(created.CoverColors) != 0 {
		t.Fatalf("created %+v", created)
	}
	if _, ok := pm.private.Get(created.Key); !ok {
		t.Fatal("no object in the private bucket")
	}
}

func TestService_LargeAnswerFileStaysRefusedWithPrivateMediaOn(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)

	_, err := pm.svc.UploadForPurpose(context.Background(), signedIn("61616161-6161-6161-6161-616161616161"), "answer_file_large", uploaded("build.pdf", "application/pdf", pdfFile()))
	if !errors.Is(err, media.ErrPurposeForbidden) && !errors.Is(err, media.ErrDirectUploadOnly) {
		t.Fatalf("err = %v", err)
	}
}

func TestService_OpenBaoDownRefusesOnlyPrivateUploads(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	pm.bao.Seal()
	p := signedIn("62626262-6262-6262-6262-626262626262")

	_, err := pm.svc.UploadForPurpose(context.Background(), p, "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	if !errors.Is(err, media.ErrPrivateUnavailable) {
		t.Fatalf("answer file: err = %v, want %v", err, media.ErrPrivateUnavailable)
	}
	if listed, _ := pm.store.List(context.Background()); len(listed) != 0 {
		t.Fatalf("%d records after the refused upload", len(listed))
	}

	picture, err := pm.svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("me.png", "image/png", pngDot()))
	if err != nil || picture.Visibility != media.VisibilityPublic || picture.URL == "" {
		t.Fatalf("profile picture while OpenBao is down: %+v, %v", picture, err)
	}
}

// zipFile is a ZIP archive of the named entries.
func zipFile(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func docxFile(t *testing.T) []byte {
	t.Helper()
	return zipFile(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`,
	})
}

func TestService_AnswerFileAcceptsADOCXByItsContent(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)

	created, err := pm.svc.UploadForPurpose(context.Background(), signedIn("63636363-6363-6363-6363-636363636363"), "answer_file", uploaded("cv.pdf", "application/pdf", docxFile(t)))
	if err != nil {
		t.Fatal(err)
	}
	if created.Type != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" || created.Kind != media.KindFile {
		t.Fatalf("created %+v", created)
	}
}

func TestService_AnswerFileRefusesAZIPThatIsNoDOCX(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)

	for name, data := range map[string][]byte{
		"plain zip":  zipFile(t, map[string]string{"notes.txt": "hello"}),
		"broken zip": []byte("PK\x03\x04 not really a zip"),
		"xlsx lookalike": zipFile(t, map[string]string{
			"[Content_Types].xml": `<Types><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`,
			"xl/workbook.xml":     `<workbook/>`,
		}),
	} {
		_, err := pm.svc.UploadForPurpose(context.Background(), signedIn("64646464-6464-6464-6464-646464646464"), "answer_file", uploaded("cv.docx", docxMIME, data))
		if !errors.Is(err, media.ErrTypeNotAllowed) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrTypeNotAllowed)
		}
	}
}

const docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// With no malware scanner, a purpose that needs a scan cannot be uploaded:
// an Answer file would never be opened before it is clean.
func TestService_PurposeThatNeedsAScanIsRefusedWithoutAScanner(t *testing.T) {
	t.Parallel()
	bao := transittest.NewServer(t)
	store, public, private := media.NewMemoryStore(), media.NewMemoryBlob(), media.NewMemoryBlob()
	svc := media.NewServiceWithOptions(store, public, authz.NewAuthorizer(authz.DefaultPolicy()), "",
		media.ServiceOptions{
			ServiceProducts: []authz.Product{authz.ProductForms},
			Private:         &media.PrivateMedia{Storage: media.NewPrivateStorage(private, transit.New(bao.Config())), AccessLog: store},
		})

	_, err := svc.UploadForPurpose(context.Background(), signedIn("65656565-6565-6565-6565-656565656565"), "answer_file", uploaded("cv.pdf", "application/pdf", pdfFile()))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrPurposeNotAvailable) || !errors.Is(err, media.ErrPurposeNeedsScanner) || !errors.As(err, &refusal) || refusal.Purpose != "answer_file" {
		t.Fatalf("err = %v", err)
	}
	if listed, _ := store.List(context.Background()); len(listed) != 0 {
		t.Fatalf("%d records", len(listed))
	}
	if bao.Logins() != 0 {
		t.Fatal("the refused upload reached OpenBao")
	}
}
