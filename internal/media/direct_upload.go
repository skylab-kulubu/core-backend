package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// deletes its pending object and aborts its multipart upload, and it no
	// longer counts as open (DirectUploadLimits.MaxOpen). It stays under
	// the account erasure's deferral horizon (the staging grace plus a day)
	// and the two days after which R2's lifecycle rule clears pending/.
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
	// DirectUploadClaimLease is how long a completion's claim holds its
	// upload. It outlasts the completion's storage work, which stops
	// directClaimMargin before it ends: every storage call has its own
	// timeout (directStorageTimeout, directJoinTimeout, directCopyTimeout),
	// and together they fit.
	DirectUploadClaimLease = 20 * time.Minute
	directClaimMargin      = 2 * time.Minute
	// directStorageTimeout bounds a storage call that moves no file: listing
	// parts, a HEAD, the ranged GET, a delete.
	directStorageTimeout = 30 * time.Second
	// directJoinTimeout bounds R2 joining up to 128 parts.
	directJoinTimeout = 3 * time.Minute
	// directCopyTimeout bounds copying the joined file (at most
	// MaxDirectUploadBytes, 2 GiB, under R2's 5 GiB CopyObject limit) to its
	// final key, inside R2.
	directCopyTimeout = 12 * time.Minute
	// directDatabaseTimeout bounds each of a completion's short
	// transactions.
	directDatabaseTimeout = 10 * time.Second
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
	// ErrDirectUploadCompleting refuses a completion (or a request for new
	// part addresses) while another completion holds the upload's claim:
	// retry in a few seconds; once that completion is done, a completion
	// answers its Media.
	ErrDirectUploadCompleting = errors.New("media: the upload is being completed")
	// ErrDirectUploadClaimLost is a completion whose claim is no longer the
	// upload's: its lease ran out before it finished.
	ErrDirectUploadClaimLost = errors.New("media: the completion's claim on the upload is lost")
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
	// Limiter is Direct upload's own budget (DirectUploadLimits), apart
	// from the single-step one. Nil limits nothing.
	Limiter *DirectUploadLimiter
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
	// ClaimUntil is when the claim of a completion under way ends; nil
	// while no completion holds the upload.
	ClaimUntil *time.Time
}

// completing reports whether a completion holds the upload at now.
func (r DirectUploadRecord) completing(now time.Time) bool {
	return r.ClaimUntil != nil && r.ClaimUntil.After(now)
}

// DirectUploadClaim is a completion's claim on an upload: the upload, the
// claim's id and lease, and the final key it copies the file to.
type DirectUploadClaim struct {
	Upload   DirectUploadRecord
	ID       uuid.UUID
	Until    time.Time
	FinalKey string
}

// DirectUploadStore keeps Direct uploads (PostgresStore). A Media store
// without it has no Direct upload.
type DirectUploadStore interface {
	UploadStagingStore
	// StageDirectUpload registers the upload and the staging row of its
	// pending object before storage opens anything at that key. With
	// maxOpen above 0, a person who has that many uploads open at now
	// (not expired) is refused with *TooManyOpenDirectUploads.
	StageDirectUpload(ctx context.Context, rec DirectUploadRecord, maxOpen int, now time.Time) error
	// SetDirectUploadMultipart records storage's multipart upload id.
	SetDirectUploadMultipart(ctx context.Context, id uuid.UUID, multipartID string) error
	// GetDirectUpload returns an upload not yet ended; ErrNotFound after.
	GetDirectUpload(ctx context.Context, id uuid.UUID) (DirectUploadRecord, error)
	// ClaimDirectUpload claims the uploader's open upload for a completion
	// in a short transaction, with a lease until until, and stages
	// finalKey for its copy. ErrDirectUploadCompleting while another claim
	// holds it; ErrNotFound when it is not the uploader's, not open, or
	// gone; ErrForbidden for an account being erased. No lock is held once
	// it returns: a completion holds none while storage works.
	ClaimDirectUpload(ctx context.Context, id, uploader uuid.UUID, finalKey string, now, until time.Time) (DirectUploadClaim, error)
	// ReleaseDirectUpload lets go of a claim that copied nothing: the upload
	// is open again.
	ReleaseDirectUpload(ctx context.Context, claim DirectUploadClaim) error
	// FinishDirectUpload creates the upload's Media (item, stored at the
	// claim's final key) with the upload's id and ends the upload, in one
	// short transaction. ErrDirectUploadClaimLost when the claim is gone.
	FinishDirectUpload(ctx context.Context, claim DirectUploadClaim, item Media, now time.Time) (Media, error)
	// EndDirectUpload ends an upload without a Media once storage was asked
	// to delete its objects: its record (unclaimed, or under claimID) and
	// the staging rows of its pending object and of finalKey go; removed
	// false leaves the staging rows ready for the sweeper instead.
	EndDirectUpload(ctx context.Context, id, claimID uuid.UUID, finalKey string, removed bool, now time.Time) error
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
	id := uuid.New()
	rec := DirectUploadRecord{
		ID: id, UploaderID: uploader, Purpose: purpose.Name, Name: req.Name, Size: req.Size,
		MaxBytes: maxBytes, Types: types, PartSize: directPartSize,
		Key: pendingKeyPrefix + id.String(), ExpiresAt: now.Add(DirectUploadTTL),
	}
	limiter := s.direct.Limiter
	if refusal := limiter.charge(uploader, id, req.Size, now); refusal != nil {
		return DirectUpload{}, refusal
	}
	var tooMany *TooManyOpenDirectUploads
	err = store.StageDirectUpload(ctx, rec, limiter.maxOpen(), now)
	switch {
	case errors.As(err, &tooMany):
		// A start the budget refuses is not charged.
		limiter.refund(id)
		return DirectUpload{}, limiter.openRefusal(tooMany, now)
	case errors.Is(err, ErrForbidden):
		// A refusal, like any 4xx: charged.
		limiter.settle(id)
		return DirectUpload{}, err
	case err != nil:
		limiter.refund(id)
		return DirectUpload{}, err
	}
	createCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	rec.MultipartID, err = storage.CreateMultipart(createCtx, rec.Key)
	cancel()
	if err == nil {
		err = store.SetDirectUploadMultipart(ctx, id, rec.MultipartID)
	}
	if err != nil {
		// Core's failure: given back.
		limiter.refund(id)
		return DirectUpload{}, s.abandonDirectUpload(ctx, store, storage, rec, err)
	}
	return s.directUploadView(ctx, storage, rec, nil)
}

func (s *service) DirectUploadParts(ctx context.Context, p authz.Principal, id uuid.UUID) (DirectUpload, error) {
	store, storage, err := s.directStorage()
	if err != nil {
		return DirectUpload{}, err
	}
	uploader, err := uuid.Parse(p.ID)
	if err != nil {
		return DirectUpload{}, ErrNotFound
	}
	rec, err := store.GetDirectUpload(ctx, id)
	if err != nil {
		return DirectUpload{}, err
	}
	if !s.ownOpenUpload(rec, uploader) {
		return DirectUpload{}, ErrNotFound
	}
	if rec.completing(s.directNow()) {
		return DirectUpload{}, ErrDirectUploadCompleting
	}
	listCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	stored, err := storage.ListParts(listCtx, rec.Key, rec.MultipartID)
	cancel()
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
	uploader, err := uuid.Parse(p.ID)
	if err != nil {
		return Media{}, ErrNotFound
	}
	// A short transaction claims the upload, with a lease; no lock and no
	// connection is held while storage works. Another completion of the
	// same upload meanwhile is told to retry (ErrDirectUploadCompleting),
	// and the account erasure defers.
	now := s.directNow()
	dbCtx, cancel := context.WithTimeout(ctx, directDatabaseTimeout)
	claim, err := store.ClaimDirectUpload(dbCtx, id, uploader, "files/"+uuid.NewString(), now, now.Add(DirectUploadClaimLease))
	cancel()
	if errors.Is(err, ErrNotFound) {
		// Completed already (its answer was lost, or this is a retry told
		// to wait), or not the caller's: the Media has the upload's id.
		return s.completedDirectUpload(ctx, uploader, id)
	}
	if err != nil {
		// ErrForbidden, an account being erased, is a refusal: the charge
		// stays, and erasure takes the upload.
		return Media{}, err
	}
	// The storage work stops before the lease ends, whatever the request
	// does meanwhile.
	work, stop := context.WithTimeout(context.WithoutCancel(ctx), DirectUploadClaimLease-directClaimMargin)
	defer stop()
	c := directCompletion{service: s, store: store, storage: storage, claim: claim, rec: claim.Upload}
	return c.run(work, p, sent)
}

// directCompletion is one completion of a claimed upload, step by step.
type directCompletion struct {
	*service
	store   DirectUploadStore
	storage MultipartStore
	claim   DirectUploadClaim
	rec     DirectUploadRecord
}

func (c directCompletion) run(ctx context.Context, p authz.Principal, sent []UploadedPart) (Media, error) {
	purpose, types, err := c.rules(p)
	if err != nil {
		return Media{}, c.refuse(ctx, err)
	}
	if err := c.joinParts(ctx, sent); err != nil {
		var sizeRefusal *DirectUploadRefusal
		switch {
		case errors.As(err, &sizeRefusal):
			return Media{}, c.refuse(ctx, err)
		case errors.Is(err, ErrNotFound):
			// The multipart upload and its object are both gone: aborted.
			return Media{}, c.refuse(ctx, err)
		}
		// The parts are not joined: the upload stays open for a retry.
		return Media{}, c.release(ctx, err)
	}
	// The parts are joined into the pending object. From here every way
	// out but a created Media ends the upload and deletes that object.
	detected, err := c.checkFile(ctx, purpose, types)
	if err != nil {
		var refusal *PurposeRefusal
		var sizeRefusal *DirectUploadRefusal
		if errors.As(err, &refusal) || errors.As(err, &sizeRefusal) {
			return Media{}, c.refuse(ctx, err)
		}
		return Media{}, c.fail(ctx, err)
	}
	copyCtx, cancel := context.WithTimeout(ctx, directCopyTimeout)
	err = c.storage.Copy(copyCtx, c.rec.Key, c.claim.FinalKey, ServingMetadata(detected, c.rec.Name))
	cancel()
	if err != nil {
		return Media{}, c.fail(ctx, err)
	}
	now := c.directNow()
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), directDatabaseTimeout)
	created, err := c.store.FinishDirectUpload(dbCtx, c.claim, Media{
		Name:                 c.rec.Name,
		Type:                 detected,
		Size:                 c.rec.Size,
		UploadedBy:           c.rec.UploaderID,
		Kind:                 KindFile,
		Purpose:              purpose.Name,
		Visibility:           VisibilityPublic,
		ExpiresAt:            pendingExpiry(purpose, now),
		CoverColors:          []string{},
		ServingPolicyApplied: true,
	}, now)
	cancel()
	switch {
	case errors.Is(err, ErrPublicationUncertain):
		// The Media may be stored: its objects stay for the staging
		// sweeper, and the pending one is a download meanwhile.
		return Media{}, err
	case errors.Is(err, ErrForbidden):
		// The uploader's account is being erased: a refusal, not core's
		// failure.
		return Media{}, c.refuse(ctx, err)
	case errors.Is(err, ErrDirectUploadClaimLost):
		// Too slow: the lease ran out. Only this completion's copy is its
		// own to delete; the upload is the sweeper's, or a new claim's.
		return Media{}, c.dropCopy(ctx, err)
	case err != nil:
		// Not the caller's fault, whatever the store's error says.
		return Media{}, c.fail(ctx, fmt.Errorf("media: store a Direct upload's Media: %v", err))
	}
	c.direct.Limiter.settle(c.rec.ID)
	c.deletePending(ctx)
	return c.withURL(created), nil
}

// rules are the purpose's rules as they are now (a deploy may have changed
// the catalogue since the upload started) narrowed by the limits the upload
// started with: the types both accept, and the declared size within the
// smaller maximum. A private purpose is refused here too: only a public
// object is copied to files/.
func (c directCompletion) rules(p authz.Principal) (Purpose, []string, error) {
	purpose, err := c.directPurpose(p, c.rec.Purpose)
	if err != nil {
		return Purpose{}, nil, err
	}
	if purpose.Visibility != VisibilityPublic {
		return Purpose{}, nil, &PurposeRefusal{Err: ErrDirectUploadPrivate, Purpose: purpose.Name}
	}
	var types []string
	for _, t := range c.rec.Types {
		if purpose.accepts(t) {
			types = append(types, t)
		}
	}
	if maxBytes := min(c.rec.MaxBytes, purpose.MaxBytes); c.rec.Size > maxBytes {
		return Purpose{}, nil, &PurposeRefusal{Err: ErrTooLarge, Purpose: purpose.Name, MaxBytes: maxBytes}
	}
	return purpose, types, nil
}

// joinParts completes the multipart upload with the parts the uploader
// sent, once they are exactly the parts the upload is split into, each
// stored whole. A multipart upload storage no longer has was joined by an
// earlier attempt when the pending object is there; ErrNotFound when it is
// not.
func (c directCompletion) joinParts(ctx context.Context, sent []UploadedPart) error {
	listCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	stored, err := c.storage.ListParts(listCtx, c.rec.Key, c.rec.MultipartID)
	cancel()
	if errors.Is(err, ErrMultipartGone) {
		return c.pendingStored(ctx)
	}
	if err != nil {
		return err
	}
	expected := directParts(c.rec.Size, c.rec.PartSize)
	byNumber := map[int32]UploadedPart{}
	storedSize := int64(0)
	for _, part := range stored {
		byNumber[part.Number] = part
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
			return &DirectUploadRefusal{DeclaredSize: c.rec.Size, Size: storedSize}
		}
	}
	joinCtx, cancel := context.WithTimeout(ctx, directJoinTimeout)
	err = c.storage.CompleteMultipart(joinCtx, c.rec.Key, c.rec.MultipartID, sent)
	cancel()
	switch {
	case errors.Is(err, ErrMultipartPartsMismatch):
		return ErrDirectUploadPartsMismatch
	case errors.Is(err, ErrMultipartGone):
		return c.pendingStored(ctx)
	}
	return err
}

// pendingStored is nil when the pending object is stored, ErrNotFound when
// it is not.
func (c directCompletion) pendingStored(ctx context.Context) error {
	headCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	defer cancel()
	_, err := c.storage.Size(headCtx, c.rec.Key)
	return err
}

// checkFile checks the joined file: its size, by a HEAD, is exactly the
// declared size, and its first bytes, by a ranged GET, name one of types.
func (c directCompletion) checkFile(ctx context.Context, purpose Purpose, types []string) (string, error) {
	headCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	size, err := c.storage.Size(headCtx, c.rec.Key)
	cancel()
	if err != nil {
		return "", err
	}
	if size != c.rec.Size {
		return "", &DirectUploadRefusal{DeclaredSize: c.rec.Size, Size: size}
	}
	readCtx, cancel := context.WithTimeout(ctx, directStorageTimeout)
	start, err := c.storage.ReadStart(readCtx, c.rec.Key, directStartBytes)
	cancel()
	if err != nil {
		return "", err
	}
	detected := detectDirectType(start)
	if detected == "" || !slices.Contains(types, detected) {
		return "", &PurposeRefusal{Err: ErrTypeNotAllowed, Purpose: purpose.Name, AllowedTypes: types}
	}
	return detected, nil
}

// refuse ends an upload whose file (or purpose) is refused. The charge
// stays, as a single-step refusal's does.
func (c directCompletion) refuse(ctx context.Context, refusal error) error {
	c.direct.Limiter.settle(c.rec.ID)
	return c.end(ctx, refusal)
}

// fail ends an upload core failed after its parts were joined: the file is
// sent again, so its charge is given back.
func (c directCompletion) fail(ctx context.Context, cause error) error {
	c.direct.Limiter.refund(c.rec.ID)
	return c.end(ctx, cause)
}

// end ends the upload without a Media: storage deletes its copy and its
// pending object (with any multipart upload open at it) first, then a short
// transaction removes the upload and its staging rows, or leaves them ready
// for the sweeper when a delete failed.
func (c directCompletion) end(ctx context.Context, cause error) error {
	base := context.WithoutCancel(ctx)
	removed := true
	for _, key := range []string{c.claim.FinalKey, c.rec.Key} {
		deleteCtx, cancel := context.WithTimeout(base, directStorageTimeout)
		if err := c.storage.Delete(deleteCtx, key); err != nil {
			removed = false
			cause = errors.Join(cause, fmt.Errorf("delete a Direct upload's object: %w", err))
		}
		cancel()
	}
	dbCtx, cancel := context.WithTimeout(base, directDatabaseTimeout)
	defer cancel()
	if err := c.store.EndDirectUpload(dbCtx, c.rec.ID, c.claim.ID, c.claim.FinalKey, removed, c.directNow()); err != nil {
		cause = errors.Join(cause, fmt.Errorf("end a Direct upload: %w", err))
	}
	return cause
}

// dropCopy deletes this completion's copy only: its claim is lost, so the
// upload and its pending object are no longer its own.
func (c directCompletion) dropCopy(ctx context.Context, cause error) error {
	base := context.WithoutCancel(ctx)
	deleteCtx, cancel := context.WithTimeout(base, directStorageTimeout)
	err := c.storage.Delete(deleteCtx, c.claim.FinalKey)
	cancel()
	if err != nil {
		// The final key's staging row, ready at the lease's end, has the
		// sweeper delete it.
		return errors.Join(cause, fmt.Errorf("delete a Direct upload's copy: %w", err))
	}
	dbCtx, cancel := context.WithTimeout(base, directDatabaseTimeout)
	defer cancel()
	if err := c.store.CancelStagedUpload(dbCtx, c.claim.FinalKey); err != nil {
		return errors.Join(cause, fmt.Errorf("end a Direct upload's copy: %w", err))
	}
	return cause
}

// release lets go of the claim when nothing was copied: the upload stays
// open for another completion.
func (c directCompletion) release(ctx context.Context, cause error) error {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), directDatabaseTimeout)
	defer cancel()
	if err := c.store.ReleaseDirectUpload(dbCtx, c.claim); err != nil {
		// The lease ends it anyway: the sweeper takes the upload then.
		return errors.Join(cause, fmt.Errorf("release a Direct upload's claim: %w", err))
	}
	return cause
}

// deletePending deletes a published upload's pending object; its staging
// row, ready already, has the sweeper retry should that fail.
func (c directCompletion) deletePending(ctx context.Context) {
	base := context.WithoutCancel(ctx)
	deleteCtx, cancel := context.WithTimeout(base, directStorageTimeout)
	err := c.storage.Delete(deleteCtx, c.rec.Key)
	cancel()
	if err != nil {
		return
	}
	dbCtx, cancel := context.WithTimeout(base, directDatabaseTimeout)
	defer cancel()
	_ = c.store.CancelStagedUpload(dbCtx, c.rec.Key)
}

// completedDirectUpload is the Media a completed upload created, for its
// uploader; ErrNotFound for anyone else or when there is none.
func (s *service) completedDirectUpload(ctx context.Context, uploader, id uuid.UUID) (Media, error) {
	m, err := s.media.Get(ctx, id)
	if err != nil || m.UploadedBy != uploader {
		return Media{}, ErrNotFound
	}
	return s.withURL(m), nil
}

// ownOpenUpload reports whether the upload is the uploader's and open:
// storage opened it and it has not expired. Anyone else's is not found, so
// a refusal tells nothing about another person's upload.
func (s *service) ownOpenUpload(rec DirectUploadRecord, uploader uuid.UUID) bool {
	return rec.UploaderID == uploader && rec.MultipartID != "" && s.directNow().Before(rec.ExpiresAt)
}

// abandonDirectUpload ends an upload that never started right: storage
// deletes its pending object (and any multipart upload open at it), then a
// short transaction removes the upload and its staging row.
func (s *service) abandonDirectUpload(ctx context.Context, store DirectUploadStore, storage MultipartStore, rec DirectUploadRecord, cause error) error {
	base := context.WithoutCancel(ctx)
	deleteCtx, cancel := context.WithTimeout(base, directStorageTimeout)
	err := storage.Delete(deleteCtx, rec.Key)
	cancel()
	removed := err == nil
	if err != nil {
		cause = errors.Join(cause, fmt.Errorf("delete a Direct upload's pending object: %w", err))
	}
	dbCtx, cancel := context.WithTimeout(base, directDatabaseTimeout)
	defer cancel()
	if err := store.EndDirectUpload(dbCtx, rec.ID, uuid.Nil, "", removed, s.directNow()); err != nil {
		cause = errors.Join(cause, fmt.Errorf("end a Direct upload: %w", err))
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

// validFileName reports whether name can be kept as a Direct upload's file
// name: not blank, at most 255 bytes, and reading as itself
// (fileNameReadsAsItself). It is stored and encoded into the download name,
// never into a key.
func validFileName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= maxDirectNameBytes && fileNameReadsAsItself(name)
}

// fileNameReadsAsItself reports whether a file name, shown to a person,
// reads as what it is: valid UTF-8 without control characters and without
// the invisible format characters (Unicode Cf: the bidirectional overrides
// and isolates U+202A–U+202E and U+2066–U+2069, the marks U+200E and U+200F,
// zero-width characters) that make a name read as another, such as
// "a\u202Epiz.exe" showing as "aexe.zip". Every upload's name must.
func fileNameReadsAsItself(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	})
}
