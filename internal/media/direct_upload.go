package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Direct upload (media redesign ticket 11, ADR-0052): a large file goes from
// the browser straight to storage as an S3 multipart upload under
// pending/<id>, through part addresses core presigns; core then checks the
// parts, the size and the file's first bytes, copies it to its final key
// with the metadata the serving policy writes, and creates the Media. See
// "Direct upload" in docs/media-lifecycle.md.

const (
	// DirectUploadTTL is how long a Direct upload may take from its start
	// to its completion. After it the upload is gone: the staging sweeper
	// deletes its pending object and aborts its multipart upload, and its
	// charge to the person's upload budget lapses. It stays under the
	// account erasure's deferral horizon (the staging grace plus a day) and
	// the two days after which R2's lifecycle rule clears pending/.
	DirectUploadTTL = 12 * time.Hour
	// DirectUploadPartURLTTL is how long a part address works. An upload
	// that takes longer asks for new ones (DirectUploadParts).
	DirectUploadPartURLTTL = time.Hour
	// directPartSize is the size of every part but the last: R2 wants them
	// equal, S3 at least 5 MiB. The largest Direct upload
	// (MaxDirectUploadBytes, 2 GiB) is 128 parts, far under S3's 10 000.
	directPartSize = 16 << 20
	// directStartBytes is how much of a Direct upload core reads (a ranged
	// GET) to tell its type.
	directStartBytes = 512
	// maxDirectNameBytes bounds the file name kept with the Media.
	maxDirectNameBytes = 255
)

var (
	// ErrSingleStepOnly refuses a single-step purpose sent to Direct upload.
	ErrSingleStepOnly = fmt.Errorf("media: the purpose uploads through POST /v1/media: %w", ErrInvalid)
	// ErrDirectUploadPrivate refuses a private purpose by Direct upload
	// until its encryption after completion exists (ticket 21). errors.Is
	// matches ErrPurposeNotAvailable.
	ErrDirectUploadPrivate = fmt.Errorf("media: a private purpose cannot be sent by Direct upload yet: %w", ErrPurposeNotAvailable)
	// ErrLimitsTooWide refuses narrowed limits wider than the purpose's: a
	// type it does not accept, or a larger maximum.
	ErrLimitsTooWide = fmt.Errorf("media: the limits are wider than the purpose's: %w", ErrInvalid)
	// ErrDirectUploadUnavailable is a core without the storage Direct upload
	// needs (no R2, or a store that cannot keep an upload).
	ErrDirectUploadUnavailable = errors.New("media: Direct upload is not available")
	// ErrDirectUploadPartsMismatch is a completion whose parts are not the
	// parts storage holds: one missing, or another ETag. Nothing is refused
	// about the file; the upload stays and can be completed with the right
	// parts.
	ErrDirectUploadPartsMismatch = fmt.Errorf("media: the parts are not the upload's: %w", ErrInvalid)
	// ErrDirectUploadSizeMismatch refuses an upload whose stored size is
	// not the size it declared (DirectUploadRefusal).
	ErrDirectUploadSizeMismatch = fmt.Errorf("media: the upload's size is not the size it declared: %w", ErrInvalid)
)

// DirectUploadRefusal is a completion refused because the stored file is
// not the file the upload declared. errors.Is matches
// ErrDirectUploadSizeMismatch.
type DirectUploadRefusal struct {
	DeclaredSize int64
	Size         int64
}

func (r *DirectUploadRefusal) Error() string {
	return fmt.Sprintf("%v (declared %d, stored %d)", ErrDirectUploadSizeMismatch, r.DeclaredSize, r.Size)
}

func (r *DirectUploadRefusal) Unwrap() error { return ErrDirectUploadSizeMismatch }

// DirectUploadConfig is what Direct upload needs besides the Media store
// (which must keep uploads: DirectUploadStore).
type DirectUploadConfig struct {
	// Storage is the public bucket's multipart side (R2). Nil: Direct
	// upload answers ErrDirectUploadUnavailable.
	Storage MultipartStore
	// Limiter is the budget single-step uploads are charged to; a Direct
	// upload is charged the size it declares. Nil charges nothing.
	Limiter *UploadLimiter
	// Now defaults to time.Now.
	Now func() time.Time
}

// DirectUploadRequest starts a Direct upload.
type DirectUploadRequest struct {
	Purpose string
	// Name is the file's name, kept with the Media and used for its
	// download name; never part of any key.
	Name string
	// Size is the file's size in bytes; the stored file must be exactly it.
	Size int64
	// Limits narrows the purpose's rules for this upload; nil keeps them.
	Limits *NarrowedLimits
}

// NarrowedLimits is what the owning product allows for one upload (a
// Skyforms question: only PDF, 5 MB). It may only narrow the purpose's:
// types it accepts, a maximum it allows.
type NarrowedLimits struct {
	Types    []string `json:"types,omitempty"`
	MaxBytes int64    `json:"maxBytes,omitempty"`
}

// DirectUpload is a started Direct upload as its uploader sees it: which
// parts storage holds, and where to send the others.
type DirectUpload struct {
	ID        uuid.UUID `json:"id"`
	Purpose   string    `json:"purpose"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	PartSize  int64     `json:"partSize"`
	PartCount int       `json:"partCount"`
	// ExpiresAt is when the upload must be completed by.
	ExpiresAt time.Time `json:"expiresAt"`
	// Uploaded are the parts storage holds already, with their ETags.
	Uploaded []UploadedPart `json:"uploaded"`
	// Parts are the parts still to send: PUT exactly Size bytes to URL.
	Parts []PartAddress `json:"parts"`
	// PartURLsExpireAt is when the part addresses stop working; ask for new
	// ones after.
	PartURLsExpireAt time.Time `json:"partUrlsExpireAt"`
}

// PartAddress is where the browser sends one part. The address carries its
// signature: it is given only to the uploader and never logged.
type PartAddress struct {
	PartNumber int32  `json:"partNumber"`
	Size       int64  `json:"size"`
	URL        string `json:"url"`
}

// DirectUploadRecord is a Direct upload core started and has not finished:
// the uploader (the staging row's subject), the purpose and the narrowed
// limits it started under, the declared size and how it is split, the
// pending object and storage's multipart upload, and the expiry.
type DirectUploadRecord struct {
	ID          uuid.UUID
	UploaderID  uuid.UUID
	Purpose     string
	Name        string
	Size        int64
	MaxBytes    int64
	Types       []string
	PartSize    int64
	Key         string
	MultipartID string
	ExpiresAt   time.Time
}

// DirectUploadStore keeps Direct uploads (PostgresStore). A Media store
// without it has no Direct upload.
type DirectUploadStore interface {
	UploadStagingStore
	// StageDirectUpload registers the upload and the staging row of its
	// pending object before storage opens anything at that key.
	StageDirectUpload(ctx context.Context, rec DirectUploadRecord) error
	// SetDirectUploadMultipart records storage's multipart upload id.
	SetDirectUploadMultipart(ctx context.Context, id uuid.UUID, multipartID string) error
	// GetDirectUpload returns an upload not yet ended; ErrNotFound after.
	GetDirectUpload(ctx context.Context, id uuid.UUID) (DirectUploadRecord, error)
	// PublishDirectUpload creates the upload's Media (item, whose object is
	// staged at its final key) with the upload's id, and ends the upload,
	// in one transaction: the final key's staging row and the upload go,
	// and the pending object is left to the cleanup. ErrNotFound when the
	// upload ended already.
	PublishDirectUpload(ctx context.Context, id uuid.UUID, item Media, now time.Time) (Media, error)
	// DropDirectUpload ends an upload without a Media; its pending object
	// is left to the cleanup. ErrNotFound when it ended already.
	DropDirectUpload(ctx context.Context, id uuid.UUID, now time.Time) error
}

func (s *service) StartDirectUpload(ctx context.Context, p authz.Principal, req DirectUploadRequest) (DirectUpload, error) {
	purpose, err := s.directPurpose(p, req.Purpose)
	if err != nil {
		return DirectUpload{}, err
	}
	uploader, err := uuid.Parse(p.ID)
	if err != nil || req.Size <= 0 || !validFileName(req.Name) {
		return DirectUpload{}, ErrInvalid
	}
	types, maxBytes, err := narrowedLimits(purpose, req.Limits)
	if err != nil {
		return DirectUpload{}, err
	}
	if req.Size > maxBytes {
		return DirectUpload{}, &PurposeRefusal{Err: ErrTooLarge, Purpose: purpose.Name, MaxBytes: maxBytes}
	}
	store, storage, err := s.directStorage()
	if err != nil {
		return DirectUpload{}, err
	}
	now := s.directNow()
	expires := now.Add(DirectUploadTTL)
	charge := UploadCharge{}
	if limiter := s.direct.Limiter; limiter != nil {
		var refusal *UploadLimitRefusal
		if charge, refusal = limiter.AdmitUntil(uploader, req.Size, expires); refusal != nil {
			return DirectUpload{}, refusal
		}
	}
	id := uuid.New()
	rec := DirectUploadRecord{
		ID: id, UploaderID: uploader, Purpose: purpose.Name, Name: req.Name, Size: req.Size,
		MaxBytes: maxBytes, Types: types, PartSize: directPartSize,
		Key: pendingKeyPrefix + id.String(), ExpiresAt: expires,
	}
	if err := store.StageDirectUpload(ctx, rec); err != nil {
		// Nothing was sent: an upload that did not start costs nothing.
		charge.Refund()
		return DirectUpload{}, err
	}
	if rec.MultipartID, err = storage.CreateMultipart(ctx, rec.Key); err == nil {
		err = store.SetDirectUploadMultipart(ctx, id, rec.MultipartID)
	}
	if err != nil {
		charge.Refund()
		return DirectUpload{}, s.endDirectUpload(ctx, store, storage, rec, err)
	}
	s.charges.hold(id, charge, expires, now)
	return s.directUploadView(ctx, storage, rec, nil)
}

func (s *service) DirectUploadParts(ctx context.Context, p authz.Principal, id uuid.UUID) (DirectUpload, error) {
	store, storage, err := s.directStorage()
	if err != nil {
		return DirectUpload{}, err
	}
	rec, err := s.ownDirectUpload(ctx, store, p, id)
	if err != nil {
		return DirectUpload{}, err
	}
	stored, err := storage.ListParts(ctx, rec.Key, rec.MultipartID)
	if errors.Is(err, ErrMultipartGone) {
		// Completed meanwhile, or aborted: nothing is left to send.
		return DirectUpload{}, ErrNotFound
	}
	if err != nil {
		return DirectUpload{}, err
	}
	return s.directUploadView(ctx, storage, rec, stored)
}

func (s *service) CompleteDirectUpload(ctx context.Context, p authz.Principal, id uuid.UUID, sent []UploadedPart) (Media, error) {
	store, storage, err := s.directStorage()
	if err != nil {
		return Media{}, err
	}
	rec, err := s.ownDirectUpload(ctx, store, p, id)
	if errors.Is(err, ErrNotFound) {
		// A completion retried after its answer was lost: the Media has the
		// upload's id.
		return s.completedDirectUpload(ctx, p, id)
	}
	if err != nil {
		return Media{}, err
	}
	// The catalogue may have changed with a deploy since the upload
	// started: its rules are those of now.
	purpose, err := s.directPurpose(p, rec.Purpose)
	if err != nil {
		return Media{}, s.refuseDirectUpload(ctx, store, storage, rec, err)
	}
	if err := s.completeParts(ctx, storage, rec, sent); err != nil {
		if errors.Is(err, ErrNotFound) {
			// The pending object is gone too: completed by another request
			// (whose Media then exists), or aborted.
			if m, lookupErr := s.completedDirectUpload(ctx, p, id); lookupErr == nil {
				return m, nil
			}
			return Media{}, s.refuseDirectUpload(ctx, store, storage, rec, ErrNotFound)
		}
		var sizeRefusal *DirectUploadRefusal
		if errors.As(err, &sizeRefusal) {
			return Media{}, s.refuseDirectUpload(ctx, store, storage, rec, err)
		}
		return Media{}, err
	}
	size, err := storage.Size(ctx, rec.Key)
	if err != nil {
		return Media{}, err
	}
	if size != rec.Size {
		return Media{}, s.refuseDirectUpload(ctx, store, storage, rec, &DirectUploadRefusal{DeclaredSize: rec.Size, Size: size})
	}
	if size > rec.MaxBytes {
		return Media{}, s.refuseDirectUpload(ctx, store, storage, rec,
			&PurposeRefusal{Err: ErrTooLarge, Purpose: purpose.Name, MaxBytes: rec.MaxBytes})
	}
	start, err := storage.ReadStart(ctx, rec.Key, directStartBytes)
	if err != nil {
		return Media{}, err
	}
	detected := detectDirectType(start)
	if detected == "" || !slices.Contains(rec.Types, detected) {
		return Media{}, s.refuseDirectUpload(ctx, store, storage, rec,
			&PurposeRefusal{Err: ErrTypeNotAllowed, Purpose: purpose.Name, AllowedTypes: rec.Types})
	}

	// The file is what the upload declared. It is copied to its final key,
	// staged like every object core writes, with the metadata the serving
	// policy writes: a ZIP (and anything but a PDF) as a download.
	now := s.directNow()
	key := "files/" + uuid.NewString()
	if err := store.StageUpload(ctx, key, rec.UploaderID, now.Add(s.uploadStagingGrace)); err != nil {
		return Media{}, err
	}
	if err := storage.Copy(ctx, rec.Key, key, ServingMetadata(detected, rec.Name)); err != nil {
		return Media{}, s.cleanupRejectedUpload(ctx, store, true, key, nil, err)
	}
	created, err := store.PublishDirectUpload(ctx, id, Media{
		Name:                 rec.Name,
		Type:                 detected,
		Size:                 size,
		UploadedBy:           rec.UploaderID,
		Kind:                 KindFile,
		Purpose:              purpose.Name,
		Visibility:           VisibilityPublic,
		ExpiresAt:            pendingExpiry(purpose, now),
		Key:                  key,
		CoverColors:          []string{},
		ServingPolicyApplied: true,
	}, now)
	switch {
	case errors.Is(err, ErrPublicationUncertain):
		return Media{}, err
	case errors.Is(err, ErrNotFound):
		// Another completion of the same upload got there first: its Media
		// stands, and this copy goes.
		cleanupErr := s.cleanupRejectedUpload(ctx, store, true, key, nil, nil)
		m, lookupErr := s.completedDirectUpload(ctx, p, id)
		if lookupErr != nil {
			return Media{}, errors.Join(lookupErr, cleanupErr)
		}
		return m, nil
	case err != nil:
		return Media{}, s.cleanupRejectedUpload(ctx, store, true, key, nil, err)
	}
	s.charges.keep(id)
	// The pending object has served its turn. Should deleting it fail, its
	// staging row is ready for the sweeper already.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if storage.Delete(cleanupCtx, rec.Key) == nil {
		_ = store.CancelStagedUpload(cleanupCtx, rec.Key)
	}
	return s.withURL(created), nil
}

// completeParts completes the upload's multipart upload with the parts the
// uploader sent, once they are exactly the parts the upload is split into,
// each stored whole. A multipart upload storage no longer has was completed
// by an earlier attempt whose answer was lost, when the pending object is
// there; ErrNotFound when it is not.
func (s *service) completeParts(ctx context.Context, storage MultipartStore, rec DirectUploadRecord, sent []UploadedPart) error {
	stored, err := storage.ListParts(ctx, rec.Key, rec.MultipartID)
	if errors.Is(err, ErrMultipartGone) {
		return s.pendingObjectStored(ctx, storage, rec)
	}
	if err != nil {
		return err
	}
	expected := directParts(rec.Size, rec.PartSize)
	byNumber := map[int32]UploadedPart{}
	for _, part := range stored {
		byNumber[part.Number] = part
	}
	storedSize := int64(0)
	for _, part := range stored {
		storedSize += part.Size
	}
	if len(sent) != len(expected) {
		return ErrDirectUploadPartsMismatch
	}
	for i, want := range expected {
		got, held := byNumber[want.PartNumber]
		if sent[i].Number != want.PartNumber || !held || got.ETag != sent[i].ETag {
			return ErrDirectUploadPartsMismatch
		}
		if got.Size != want.Size {
			// Part addresses are signed for their size, so storage should
			// never hold another; a file that differs from its declaration
			// is refused, whatever sent it.
			return &DirectUploadRefusal{DeclaredSize: rec.Size, Size: storedSize}
		}
	}
	err = storage.CompleteMultipart(ctx, rec.Key, rec.MultipartID, sent)
	switch {
	case errors.Is(err, ErrMultipartPartsMismatch):
		return ErrDirectUploadPartsMismatch
	case errors.Is(err, ErrMultipartGone):
		return s.pendingObjectStored(ctx, storage, rec)
	}
	return err
}

// pendingObjectStored reports whether the upload's pending object is stored
// (nil) or not (ErrNotFound).
func (s *service) pendingObjectStored(ctx context.Context, storage MultipartStore, rec DirectUploadRecord) error {
	_, err := storage.Size(ctx, rec.Key)
	return err
}

// completedDirectUpload is the Media a completed upload created, for its
// uploader; ErrNotFound for anyone else or when there is none.
func (s *service) completedDirectUpload(ctx context.Context, p authz.Principal, id uuid.UUID) (Media, error) {
	uploader, err := uuid.Parse(p.ID)
	if err != nil {
		return Media{}, ErrNotFound
	}
	m, err := s.media.Get(ctx, id)
	if err != nil || m.UploadedBy != uploader {
		return Media{}, ErrNotFound
	}
	return s.withURL(m), nil
}

// ownDirectUpload is the caller's upload, not yet expired; ErrNotFound for
// anyone else's, so a refusal tells nothing about another person's upload.
func (s *service) ownDirectUpload(ctx context.Context, store DirectUploadStore, p authz.Principal, id uuid.UUID) (DirectUploadRecord, error) {
	uploader, err := uuid.Parse(p.ID)
	if err != nil {
		return DirectUploadRecord{}, ErrNotFound
	}
	rec, err := store.GetDirectUpload(ctx, id)
	if err != nil {
		return DirectUploadRecord{}, err
	}
	if rec.UploaderID != uploader || rec.MultipartID == "" || !s.directNow().Before(rec.ExpiresAt) {
		return DirectUploadRecord{}, ErrNotFound
	}
	return rec, nil
}

// refuseDirectUpload ends an upload whose file is refused (or whose
// purpose no longer takes it): its record goes, its pending object and
// multipart upload are deleted, and its charge is given back. The refusal
// is returned with anything that failed on the way; the staging sweeper
// finishes what did not.
func (s *service) refuseDirectUpload(ctx context.Context, store DirectUploadStore, storage MultipartStore, rec DirectUploadRecord, refusal error) error {
	s.charges.refund(rec.ID)
	return s.endDirectUpload(ctx, store, storage, rec, refusal)
}

// endDirectUpload ends an upload without a Media: its record goes, then its
// pending object (with the multipart upload open at it) and its staging
// row. A step that fails leaves the staging row ready for the sweeper.
func (s *service) endDirectUpload(ctx context.Context, store DirectUploadStore, storage MultipartStore, rec DirectUploadRecord, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := store.DropDirectUpload(cleanupCtx, rec.ID, s.directNow()); err != nil && !errors.Is(err, ErrNotFound) {
		return errors.Join(cause, fmt.Errorf("end Direct upload: %w", err))
	}
	if err := storage.Delete(cleanupCtx, rec.Key); err != nil {
		return errors.Join(cause, fmt.Errorf("delete Direct upload's pending object: %w", err))
	}
	if err := store.CancelStagedUpload(cleanupCtx, rec.Key); err != nil {
		return errors.Join(cause, fmt.Errorf("complete Direct upload cleanup: %w", err))
	}
	return cause
}

// directUploadView presigns an address for every part storage does not
// hold whole yet.
func (s *service) directUploadView(ctx context.Context, storage MultipartStore, rec DirectUploadRecord, stored []UploadedPart) (DirectUpload, error) {
	expected := directParts(rec.Size, rec.PartSize)
	held := map[int32]UploadedPart{}
	for _, part := range stored {
		held[part.Number] = part
	}
	view := DirectUpload{
		ID: rec.ID, Purpose: rec.Purpose, Name: rec.Name, Size: rec.Size, PartSize: rec.PartSize,
		PartCount: len(expected), ExpiresAt: rec.ExpiresAt,
		Uploaded: []UploadedPart{}, Parts: []PartAddress{},
		PartURLsExpireAt: s.directNow().Add(DirectUploadPartURLTTL),
	}
	for _, part := range expected {
		if got, ok := held[part.PartNumber]; ok && got.Size == part.Size {
			view.Uploaded = append(view.Uploaded, got)
			continue
		}
		address, err := storage.PresignPart(ctx, rec.Key, rec.MultipartID, part.PartNumber, part.Size, DirectUploadPartURLTTL)
		if err != nil {
			return DirectUpload{}, err
		}
		part.URL = address
		view.Parts = append(view.Parts, part)
	}
	return view, nil
}

// directPurpose is the purpose the caller may send by Direct upload now,
// or the rule that refuses it, checked in the order single-step uploads
// check theirs.
func (s *service) directPurpose(p authz.Principal, name string) (Purpose, error) {
	purpose, ok := s.addresses.Catalogue.Lookup(name)
	if !ok || name == PurposeLegacy {
		return Purpose{}, &PurposeRefusal{Err: ErrPurposeUnknown, Purpose: name}
	}
	if err := s.purposeRefusal(p, purpose, TransportDirect); err != nil {
		return Purpose{}, err
	}
	return purpose, nil
}

func (s *service) directStorage() (DirectUploadStore, MultipartStore, error) {
	store, ok := s.media.(DirectUploadStore)
	if !ok || s.direct.Storage == nil {
		return nil, nil, ErrDirectUploadUnavailable
	}
	return store, s.direct.Storage, nil
}

func (s *service) directNow() time.Time {
	if s.direct.Now != nil {
		return s.direct.Now().UTC()
	}
	return time.Now().UTC()
}

// narrowedLimits intersects the purpose's rules with the owning product's
// narrower ones: the types both accept and the smaller maximum. Anything
// wider than the purpose is refused, never silently cut.
func narrowedLimits(purpose Purpose, limits *NarrowedLimits) ([]string, int64, error) {
	types, maxBytes := slices.Clone(purpose.Types), purpose.MaxBytes
	if limits == nil {
		return types, maxBytes, nil
	}
	tooWide := &PurposeRefusal{Err: ErrLimitsTooWide, Purpose: purpose.Name, AllowedTypes: purpose.Types, MaxBytes: purpose.MaxBytes}
	if limits.MaxBytes < 0 || limits.MaxBytes > purpose.MaxBytes {
		return nil, 0, tooWide
	}
	if limits.MaxBytes > 0 {
		maxBytes = limits.MaxBytes
	}
	if len(limits.Types) > 0 {
		types = nil
		for _, t := range limits.Types {
			if !purpose.accepts(t) {
				return nil, 0, tooWide
			}
			if !slices.Contains(types, t) {
				types = append(types, t)
			}
		}
	}
	return types, maxBytes, nil
}

// directParts are the parts a file of size bytes is split into: every part
// but the last is partSize.
func directParts(size, partSize int64) []PartAddress {
	count := (size + partSize - 1) / partSize
	parts := make([]PartAddress, 0, count)
	for i := int64(0); i < count; i++ {
		parts = append(parts, PartAddress{PartNumber: int32(i + 1), Size: min(partSize, size-i*partSize)})
	}
	return parts
}

// detectDirectType names a Direct upload's type from its first bytes: PDF
// by its header, ZIP by its first local file header (or the end of an
// empty archive), MP4 by its ftyp box. Only these may be a Direct upload
// purpose's types (ErrCeilingDirectType). "" for anything else.
func detectDirectType(start []byte) string {
	switch {
	case bytes.HasPrefix(start, []byte("%PDF-")):
		return pdfType
	case bytes.HasPrefix(start, []byte("PK\x03\x04")), bytes.HasPrefix(start, []byte("PK\x05\x06")):
		return zipType
	case len(start) >= 16 && string(start[4:8]) == "ftyp" &&
		(uint32(start[0])<<24|uint32(start[1])<<16|uint32(start[2])<<8|uint32(start[3])) >= 16:
		// An ftyp box: its size, "ftyp", a major brand and a minor version.
		// Where its moov box sits is for video's own ticket (13).
		return mp4Type
	}
	return ""
}

// validFileName reports whether name can be kept as the file's name: text
// of at most 255 bytes, no control characters. It is stored and encoded
// into the download name, never into a key.
func validFileName(name string) bool {
	if strings.TrimSpace(name) == "" || len(name) > maxDirectNameBytes || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

// directCharges holds each started upload's charge to its person's budget
// until the upload ends: kept once it completes, given back when its file
// is refused. One never completed lapses by itself when the upload expires
// (UploadLimiter.AdmitUntil); holding it longer changes nothing.
type directCharges struct {
	mu      sync.Mutex
	charges map[uuid.UUID]heldCharge
}

type heldCharge struct {
	charge UploadCharge
	lapses time.Time
}

func (c *directCharges) hold(id uuid.UUID, charge UploadCharge, lapses, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.charges == nil {
		c.charges = map[uuid.UUID]heldCharge{}
	}
	// Uploads that expired have nothing left to keep or give back.
	for other, held := range c.charges {
		if !held.lapses.After(now) {
			delete(c.charges, other)
		}
	}
	c.charges[id] = heldCharge{charge: charge, lapses: lapses}
}

func (c *directCharges) take(id uuid.UUID) (UploadCharge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.charges[id]
	delete(c.charges, id)
	return held.charge, ok
}

func (c *directCharges) keep(id uuid.UUID) {
	if charge, ok := c.take(id); ok {
		charge.Keep()
	}
}

func (c *directCharges) refund(id uuid.UUID) {
	if charge, ok := c.take(id); ok {
		charge.Refund()
	}
}
