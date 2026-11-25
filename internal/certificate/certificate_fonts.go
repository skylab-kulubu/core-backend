package certificate

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Font is a team's or the club's certificate font (docs/certificate-fonts.md):
// one face (family, weight, style) whose checked WOFF2 is the Media MediaID.
// A text element uses it by that Media (Element.FontMediaID).
type Font struct {
	ID uuid.UUID `json:"id"`
	// OwnerTeam is the team whose templates use it; "" for the club's,
	// which every template may use.
	OwnerTeam string    `json:"ownerTeam"`
	Family    string    `json:"family"`
	Weight    int       `json:"weight"`
	Style     string    `json:"style"` // FontStyleNormal or FontStyleItalic
	MediaID   uuid.UUID `json:"mediaId"`
	// MissingTurkish is the letters (of ğĞıİşŞçÇöÖüÜ, A–Z, a–z, 0–9) the
	// font lacks; "" when it has them all. A letter it lacks prints in the
	// element's fallback.
	MissingTurkish string     `json:"missingTurkish"`
	UploadedBy     *uuid.UUID `json:"uploadedBy,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	ArchivedAt     *time.Time `json:"archivedAt,omitempty"`
}

// FontInput adds a certificate font (POST /v1/certificate-fonts): the
// caller's certificate_font upload and the team it is for ("" for the
// club).
type FontInput struct {
	MediaID   uuid.UUID `json:"mediaId"`
	OwnerTeam string    `json:"ownerTeam"`
}

// FontStyleNormal is an upright font (Font.Style).
const FontStyleNormal = "normal"

// The bounds of a certificate font.
const (
	// maxFontBytes bounds the upload and the checked WOFF2 (the
	// certificate_font purpose's max_mib).
	maxFontBytes       = 10 << 20
	maxFontFamilyRunes = 80
)

// The codes of a certificate font (docs/certificate-template-errors.md).
const (
	CodeFontUnreadable          = "font_unreadable"
	CodeFontTooLarge            = "font_too_large"
	CodeFontUnsupported         = "font_unsupported"
	CodeFontEmbeddingRestricted = "font_embedding_restricted"
	CodeFontExists              = "font_exists"
	CodeLayoutFontOutOfScope    = "layout_font_out_of_scope"
)

// FontExistsError is a face the team (or the club) has already: the same
// family (in any case), weight and style among its current fonts (409
// font_exists). FontID names it.
type FontExistsError struct{ FontID uuid.UUID }

func (e *FontExistsError) Error() string {
	return "certificate: font exists: " + e.FontID.String()
}

// FontStore keeps the certificate fonts; PostgresStore and MemoryStore do.
type FontStore interface {
	// ListFonts lists the current fonts of the teams named ("" for the
	// club's), by team, family, weight and style.
	ListFonts(ctx context.Context, teams []string) ([]Font, error)
	// GetFont reads a font, archived or not.
	GetFont(ctx context.Context, id uuid.UUID) (Font, error)
	// FontsByMedia reads the fonts whose file is one of ids, archived or not.
	FontsByMedia(ctx context.Context, ids []uuid.UUID) ([]Font, error)
	// CreateFont answers ErrConflict for a face the team has already.
	CreateFont(ctx context.Context, f Font) (Font, error)
	// ArchiveFont archives a font (its row and its file stay).
	ArchiveFont(ctx context.Context, id uuid.UUID, at time.Time) (Font, error)
}

// FontSanitizer checks a font someone uploaded and writes it as WOFF2
// (pdf-tools' /v1/font/sanitize); PDFToolsClient is one.
type FontSanitizer interface {
	SanitizeFont(ctx context.Context, font []byte) (SanitizedFont, error)
}

// SanitizedFont is what pdf-tools says of a font it checked, and the font
// as WOFF2.
type SanitizedFont struct {
	Family         string `json:"family"`
	Subfamily      string `json:"subfamily"`
	Weight         int    `json:"weight"`
	Italic         bool   `json:"italic"`
	GlyphCount     int    `json:"glyphCount"`
	MissingTurkish string `json:"missingTurkish"`
	FSType         int    `json:"fsType"`
	WOFF2          []byte `json:"-"`
}

// woff2Signature opens every WOFF2 file.
var woff2Signature = []byte("wOF2")

func (s *service) fontSanitizer() FontSanitizer {
	sanitizer, _ := s.pdfTools.(FontSanitizer)
	return sanitizer
}

// mayManageFonts is whether p may add or archive team's fonts: who may
// create its templates (a Leader, certificate:template:manage); for the
// club's (""), who may create a club-wide template (Privileged).
func (s *service) mayManageFonts(p authz.Principal, team string) bool {
	return s.authz.Allow(p, templateResource(team), authz.Create)
}

// mayUseFonts is whether p may list team's fonts, read their files and set
// text in them: who may read or edit its templates, or edit its Events
// (an Event template is its Event editors'). The club's ("") are for
// anyone who may make a template or an Event template for some team.
func (s *service) mayUseFonts(p authz.Principal, team string) bool {
	if team == "" {
		return s.authz.Allow(p, templateResource(""), authz.Read) ||
			s.authz.Allow(p, authz.Resource{Type: authz.TypeMedia, MediaUploader: authz.MediaUploaderCertificateTemplateEditor, MediaOwner: authz.ProductCore}, authz.Upload)
	}
	return s.authz.Allow(p, templateResource(team), authz.Read) ||
		s.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: team}, authz.Update)
}

func (s *service) ListFonts(ctx context.Context, p authz.Principal, team string) ([]Font, error) {
	if s.fonts == nil {
		return nil, ErrInvalid
	}
	team = normalizeTeam(team)
	teams := []string{}
	if team != "" {
		if !s.mayUseFonts(p, team) {
			return nil, ErrForbidden
		}
		teams = append(teams, team)
	}
	if s.mayUseFonts(p, "") {
		teams = append(teams, "")
	}
	if len(teams) == 0 {
		return nil, ErrForbidden
	}
	return s.fonts.ListFonts(ctx, teams)
}

func (s *service) CreateFont(ctx context.Context, p authz.Principal, in FontInput) (Font, error) {
	if s.fonts == nil || in.MediaID == uuid.Nil {
		return Font{}, ErrInvalid
	}
	team := normalizeTeam(in.OwnerTeam)
	if !s.mayManageFonts(p, team) {
		return Font{}, ErrForbidden
	}
	sanitizer := s.fontSanitizer()
	if sanitizer == nil {
		return Font{}, pdfToolsUnavailable(nil)
	}
	if s.uploads == nil {
		return Font{}, invalid(CodeAssetStorageUnavailable)
	}
	raw, err := s.uploadedFont(ctx, p, in.MediaID)
	if err != nil {
		return Font{}, err
	}
	checked, err := sanitizer.SanitizeFont(ctx, raw)
	if err != nil {
		return Font{}, err
	}
	// pdf-tools' answer is trusted no more than the upload (SPEC §6.3).
	if !bytes.HasPrefix(checked.WOFF2, woff2Signature) {
		return Font{}, pdfToolsUnavailable(errors.New("pdf-tools: font/sanitize answered no WOFF2"))
	}
	if len(checked.WOFF2) > maxFontBytes {
		return Font{}, invalid(CodeFontTooLarge)
	}
	face := Font{
		ID: uuid.New(), OwnerTeam: team, Family: fontFamilyName(checked.Family), Weight: checked.Weight, Style: FontStyleNormal,
		MissingTurkish: truncateRunes(checked.MissingTurkish, 200), UploadedBy: principalUUIDPtr(p),
	}
	if face.Family == "" {
		return Font{}, invalid(CodeFontUnreadable)
	}
	if face.Weight < 1 || face.Weight > 1000 {
		face.Weight = 400
	}
	if checked.Italic {
		face.Style = FontStyleItalic
	}
	// Told before the file is stored; the unique index tells a race.
	if existing, err := s.sameFace(ctx, face); err != nil {
		return Font{}, err
	} else if existing != nil {
		return Font{}, &FontExistsError{FontID: existing.ID}
	}
	stored, err := s.uploads.UploadForPurpose(ctx, p, media.PurposeCertificateFont, media.UploadedFile{
		Name: fontFileName(face), ContentType: "font/woff2", Data: checked.WOFF2,
	})
	if err != nil {
		return Font{}, uploadRefusal(err)
	}
	face.MediaID = stored.ID
	created, err := s.fonts.CreateFont(ctx, face)
	if errors.Is(err, ErrConflict) {
		if existing, lookupErr := s.sameFace(ctx, face); lookupErr == nil && existing != nil {
			return Font{}, &FontExistsError{FontID: existing.ID}
		}
	}
	return created, err
}

// uploadedFont reads the caller's own certificate_font upload. Another's,
// or a Media of another purpose, is not found: as a Media link, it does
// not say the Media exists.
func (s *service) uploadedFont(ctx context.Context, p authz.Principal, id uuid.UUID) ([]byte, error) {
	origins, ok := s.assets.(assetOrigins)
	if !ok {
		return nil, invalid(CodeAssetStorageUnavailable)
	}
	origin, err := origins.AssetOrigin(ctx, id)
	if err != nil {
		return nil, err
	}
	caller := principalUUIDPtr(p)
	if origin.Purpose != media.PurposeCertificateFont || caller == nil || origin.UploadedBy != *caller {
		return nil, ErrNotFound
	}
	asset, err := s.decrypted(s.assets).ReadAsset(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case len(asset.Data) == 0:
		return nil, invalid(CodeFontUnreadable)
	case len(asset.Data) > maxFontBytes:
		return nil, invalid(CodeFontTooLarge)
	}
	return asset.Data, nil
}

// sameFace is the team's current font of f's face, nil for none.
func (s *service) sameFace(ctx context.Context, f Font) (*Font, error) {
	current, err := s.fonts.ListFonts(ctx, []string{f.OwnerTeam})
	if err != nil {
		return nil, err
	}
	for _, other := range current {
		if other.OwnerTeam == f.OwnerTeam && strings.EqualFold(other.Family, f.Family) && other.Weight == f.Weight && other.Style == f.Style {
			return &other, nil
		}
	}
	return nil, nil
}

// fontFamilyName is a family name as a font row keeps it: spaces folded,
// at most 80 characters.
func fontFamilyName(family string) string {
	family = strings.Join(strings.Fields(strings.ToValidUTF8(family, "")), " ")
	return strings.TrimSpace(truncateRunes(family, maxFontFamilyRunes))
}

// fontFileName is the checked file's name: "Family-700-italic.woff2".
func fontFileName(f Font) string {
	name := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', '"', ':', '*', '?', '<', '>', '|':
			return '-'
		}
		return r
	}, f.Family)
	return name + "-" + strconv.Itoa(f.Weight) + "-" + f.Style + ".woff2"
}

// FontFile is a certificate font's checked file (GET
// /v1/certificate-fonts/{id}/file).
type FontFile struct {
	Data []byte
}

func (s *service) FontFile(ctx context.Context, p authz.Principal, id uuid.UUID) (FontFile, error) {
	if s.fonts == nil {
		return FontFile{}, ErrInvalid
	}
	font, err := s.fonts.GetFont(ctx, id)
	if err != nil {
		return FontFile{}, err
	}
	if !s.mayUseFonts(p, font.OwnerTeam) {
		return FontFile{}, ErrNotFound
	}
	data, err := readFontFile(ctx, s.decrypted(s.assets), font.MediaID)
	if err != nil {
		return FontFile{}, err
	}
	return FontFile{Data: data}, nil
}

func (s *service) ArchiveFont(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if s.fonts == nil {
		return ErrInvalid
	}
	font, err := s.fonts.GetFont(ctx, id)
	if err != nil {
		return err
	}
	if !s.mayUseFonts(p, font.OwnerTeam) && !s.mayManageFonts(p, font.OwnerTeam) {
		return ErrNotFound
	}
	if !s.mayManageFonts(p, font.OwnerTeam) {
		return ErrForbidden
	}
	if font.ArchivedAt != nil {
		return nil
	}
	_, err = s.fonts.ArchiveFont(ctx, id, time.Now().UTC())
	return err
}

// readFontFile reads a certificate font's checked file: WOFF2 only, at most
// 10 MiB.
func readFontFile(ctx context.Context, assets AssetReader, id uuid.UUID) ([]byte, error) {
	if assets == nil {
		return nil, invalid(CodeAssetStorageUnavailable)
	}
	asset, err := assets.ReadAsset(ctx, id)
	if errors.Is(err, media.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case mediaType(asset.ContentType) != "font/woff2" || (len(asset.Data) > 0 && !bytes.HasPrefix(asset.Data, woff2Signature)):
		return nil, invalid(CodeAssetTypeUnsupported)
	case len(asset.Data) == 0:
		return nil, invalid(CodeAssetEmpty)
	case len(asset.Data) > maxFontBytes:
		return nil, invalid(CodeAssetTooLarge)
	}
	return asset.Data, nil
}

// uploadedFontFamily is the page's font-family of a certificate font.
func uploadedFontFamily(mediaID uuid.UUID) string {
	return "cf-" + mediaID.String()
}

// fontFace is one @font-face rule and what tells it from another.
type fontFace struct {
	key string
	css string
}

// uploadedFontFace is the @font-face of the certificate font an element
// uses, at the element's weight and style: the face is the file itself, so
// the browser never makes a bolder or slanted copy of it. files keeps the
// data URIs read so far (one read a file).
func uploadedFontFace(ctx context.Context, assets AssetReader, mediaID uuid.UUID, el Element, files map[uuid.UUID]string) (fontFace, error) {
	uri, ok := files[mediaID]
	if !ok {
		data, err := readFontFile(ctx, assets, mediaID)
		if err != nil {
			return fontFace{}, err
		}
		uri = "data:font/woff2;base64," + base64.StdEncoding.EncodeToString(data)
		files[mediaID] = uri
	}
	weight := el.FontWeight
	if weight == 0 {
		weight = 400
	}
	style := FontStyleNormal
	if el.FontStyle == FontStyleItalic {
		style = FontStyleItalic
	}
	family := uploadedFontFamily(mediaID)
	return fontFace{
		key: family + "|" + strconv.Itoa(weight) + "|" + style,
		css: `@font-face{font-family:'` + family + `';font-style:` + style + `;font-weight:` + strconv.Itoa(weight) +
			`;src:url(` + uri + `) format('woff2')}`,
	}, nil
}

// layoutFontIDs are the certificate fonts a layout's elements use.
func layoutFontIDs(layout Layout) []uuid.UUID {
	var ids []uuid.UUID
	for _, el := range layout.Elements {
		if el.FontMediaID != nil && !slices.Contains(ids, *el.FontMediaID) {
			ids = append(ids, *el.FontMediaID)
		}
	}
	return ids
}

// checkLayoutFonts refuses a certificate font the layout links that the
// previous draft did not, unless it is a current font of ownerTeam or of
// the club (layout_font_out_of_scope). A font the draft has already stays,
// archived or not.
func (s *service) checkLayoutFonts(ctx context.Context, layout, previous Layout, ownerTeam string) error {
	before := layoutFontIDs(previous)
	var added []uuid.UUID
	for _, id := range layoutFontIDs(layout) {
		if !slices.Contains(before, id) {
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil
	}
	if s.fonts == nil {
		return invalid(CodeLayoutFontOutOfScope)
	}
	rows, err := s.fonts.FontsByMedia(ctx, added)
	if err != nil {
		return err
	}
	team := normalizeTeam(ownerTeam)
	for _, id := range added {
		inScope := false
		for _, row := range rows {
			if row.MediaID == id && row.ArchivedAt == nil && (row.OwnerTeam == "" || row.OwnerTeam == team) {
				inScope = true
				break
			}
		}
		if !inScope {
			return invalid(CodeLayoutFontOutOfScope)
		}
	}
	return nil
}

// usableFonts are the current fonts of the template's team and the club,
// as a text conversion matches the PDF's fonts against them (SPEC §5.3
// step 1): the club's alone without a template, none without fonts.
func (s *service) usableFonts(ctx context.Context, p authz.Principal, templateID *uuid.UUID) ([]Font, error) {
	if s.fonts == nil {
		return nil, nil
	}
	teams := []string{""}
	if templateID != nil {
		if s.templates == nil {
			return nil, ErrInvalid
		}
		item, err := s.templates.GetTemplate(ctx, *templateID)
		if err != nil {
			return nil, err
		}
		if err := s.allowTemplate(ctx, p, item, authz.Update); err != nil {
			return nil, err
		}
		if team := normalizeTeam(item.OwnerTeam); team != "" {
			teams = append([]string{team}, teams...)
		}
	}
	return s.fonts.ListFonts(ctx, teams)
}

// validFontStyle is a font row's style.
func validFontStyle(style string) bool {
	return style == FontStyleNormal || style == FontStyleItalic
}

// validFont is a font row a store may keep.
func validFont(f Font) bool {
	n := utf8.RuneCountInString(f.Family)
	return f.ID != uuid.Nil && f.MediaID != uuid.Nil && n >= 1 && n <= maxFontFamilyRunes &&
		f.Weight >= 1 && f.Weight <= 1000 && validFontStyle(f.Style)
}
