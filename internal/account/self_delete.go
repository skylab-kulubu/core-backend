package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	ErrSelfDeletionDisabled              = errors.New("self deletion disabled")
	ErrSelfDeletionUnavailable           = errors.New("self deletion unavailable")
	ErrSelfDeletionInvalidIdempotencyKey = errors.New("invalid self deletion idempotency key")
	ErrSelfDeletionIdempotencyConflict   = errors.New("self deletion idempotency conflict")
	ErrSelfDeletionReceiptNotFound       = errors.New("self deletion receipt not found")
	// ErrSelfDeletionNotAccepted means Replay has nothing to answer: Core
	// never confirmed a request under this key for this subject, so the call
	// is a new command and needs a live proof.
	ErrSelfDeletionNotAccepted = errors.New("self deletion key not accepted")
)

type SelfDeletionStatus string

const (
	SelfDeletionBlocking           SelfDeletionStatus = "blocking"
	SelfDeletionPending            SelfDeletionStatus = "pending"
	SelfDeletionProcessing         SelfDeletionStatus = "processing"
	SelfDeletionCompleted          SelfDeletionStatus = "completed"
	SelfDeletionManualIntervention SelfDeletionStatus = "manual_intervention"
)

type SelfDeletionView struct {
	Receipt          string
	Status           SelfDeletionStatus
	Partial          bool
	PlatformBlocked  bool
	RequestedAt      time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	ReceiptExpiresAt time.Time
}

type SelfDeletionRecord = user.SelfDeletionRecord

type SelfDeletionStore interface {
	RequestSelfDeletion(
		ctx context.Context,
		subjectID uuid.UUID,
		intake user.SelfDeletionIntake,
	) (user.SelfDeletionRecord, error)
	SelfDeletionByReceiptLookup(ctx context.Context, receiptLookupHash [sha256.Size]byte) (user.SelfDeletionRecord, error)
	RetrySelfDeletion(ctx context.Context, receiptLookupHash [sha256.Size]byte, now time.Time) (user.SelfDeletionRecord, error)
}

type SelfDeletionConfig struct {
	Enabled    bool
	ReceiptKey []byte
	ReceiptTTL time.Duration
}

type SelfDeletionProjector interface {
	Project(context.Context, user.DeletionRequest) error
}

type SelfDeletion struct {
	store     SelfDeletionStore
	projector SelfDeletionProjector
	config    SelfDeletionConfig
	now       func() time.Time
}

func NewSelfDeletion(store SelfDeletionStore, projector SelfDeletionProjector, config SelfDeletionConfig, clocks ...func() time.Time) (*SelfDeletion, error) {
	if store == nil {
		return nil, errors.New("self deletion store is required")
	}
	if config.Enabled {
		if projector == nil {
			return nil, errors.New("self deletion access projector is required when enabled")
		}
		if len(config.ReceiptKey) < 32 {
			return nil, errors.New("self deletion receipt key must contain at least 32 bytes")
		}
		if config.ReceiptTTL <= 0 {
			return nil, errors.New("self deletion receipt TTL must be positive")
		}
	}
	now := time.Now
	if len(clocks) > 0 && clocks[0] != nil {
		now = clocks[0]
	}
	return &SelfDeletion{store: store, projector: projector, config: config, now: now}, nil
}

func (s *SelfDeletion) Begin(ctx context.Context, subjectID uuid.UUID, idempotencyKey string) (SelfDeletionView, error) {
	if !s.config.Enabled {
		return SelfDeletionView{}, ErrSelfDeletionDisabled
	}
	if subjectID == uuid.Nil || !validIdempotencyKey(idempotencyKey) {
		return SelfDeletionView{}, ErrSelfDeletionInvalidIdempotencyKey
	}
	now := s.now().UTC()
	idempotencyHash := scopedIdempotencyHash(subjectID, idempotencyKey)
	receipt := s.receipt(subjectID, idempotencyHash)
	lookupHash, receiptHash := receiptDigests(receipt)
	record, err := s.store.RequestSelfDeletion(ctx, subjectID, user.SelfDeletionIntake{
		IdempotencyHash: idempotencyHash, ReceiptLookupHash: lookupHash, ReceiptHash: receiptHash,
		ReceiptExpiresAt: now.Add(s.config.ReceiptTTL), CreatedAt: now,
	})
	if err != nil {
		if errors.Is(err, user.ErrSelfDeletionIdempotencyConflict) {
			return SelfDeletionView{}, ErrSelfDeletionIdempotencyConflict
		}
		return SelfDeletionView{}, err
	}
	if !validSelfDeletionRecord(record, receiptHash, now) {
		return SelfDeletionView{}, ErrSelfDeletionIdempotencyConflict
	}
	if err := s.projector.Project(ctx, record.Request); err != nil {
		return SelfDeletionView{}, ErrSelfDeletionUnavailable
	}
	record, err = s.store.SelfDeletionByReceiptLookup(ctx, lookupHash)
	if err != nil || record.Request.PlatformBlockedAt == nil || !validSelfDeletionRecord(record, receiptHash, s.now().UTC()) {
		return SelfDeletionView{}, ErrSelfDeletionUnavailable
	}
	view := selfDeletionView(record)
	view.Receipt = receipt
	return view, nil
}

// Replay answers a repeat of an idempotency key Core already accepted for
// this subject with that request's current outcome, receipt included: the
// answer Begin gives the same repeat. It is read-only and needs no proof of
// recent authentication, because the deletion saga closes the Keycloak
// session a sudo proof is bound to and the realm then calls that proof
// inactive, while Account Center may still be retrying a lost answer.
//
// "Accepted" means Core answered the key with success, or could have: an
// intake for exactly this subject and key whose global block is confirmed,
// with a live receipt. Anything else - an unknown or malformed key, another
// subject, an unconfirmed, revoked or expired intake, or the feature being
// off - is ErrSelfDeletionNotAccepted, and the caller must present a proof.
// Before the block is confirmed the saga cannot have started, so the proof
// is still good for Begin to finish the request.
func (s *SelfDeletion) Replay(ctx context.Context, subjectID uuid.UUID, idempotencyKey string) (SelfDeletionView, error) {
	if !s.config.Enabled || subjectID == uuid.Nil || !validIdempotencyKey(idempotencyKey) {
		return SelfDeletionView{}, ErrSelfDeletionNotAccepted
	}
	idempotencyHash := scopedIdempotencyHash(subjectID, idempotencyKey)
	receipt := s.receipt(subjectID, idempotencyHash)
	lookupHash, receiptHash := receiptDigests(receipt)
	record, err := s.store.SelfDeletionByReceiptLookup(ctx, lookupHash)
	if errors.Is(err, user.ErrNotFound) {
		return SelfDeletionView{}, ErrSelfDeletionNotAccepted
	}
	if err != nil {
		return SelfDeletionView{}, err
	}
	// The receipt already binds subject and key; the explicit comparisons
	// keep a store bug from ever answering for someone else.
	if record.Request.SubjectID != subjectID ||
		subtle.ConstantTimeCompare(record.IdempotencyHash[:], idempotencyHash[:]) != 1 ||
		record.Request.PlatformBlockedAt == nil ||
		!validSelfDeletionRecord(record, receiptHash, s.now().UTC()) {
		return SelfDeletionView{}, ErrSelfDeletionNotAccepted
	}
	view := selfDeletionView(record)
	view.Receipt = receipt
	return view, nil
}

func (s *SelfDeletion) Status(ctx context.Context, receipt string) (SelfDeletionView, error) {
	record, err := s.recordForReceipt(ctx, receipt)
	if err != nil {
		return SelfDeletionView{}, err
	}
	return selfDeletionView(record), nil
}

func (s *SelfDeletion) Retry(ctx context.Context, receipt string) (SelfDeletionView, error) {
	if !s.config.Enabled {
		return SelfDeletionView{}, ErrSelfDeletionDisabled
	}
	record, err := s.recordForReceipt(ctx, receipt)
	if err != nil {
		return SelfDeletionView{}, err
	}
	if record.Request.PlatformBlockedAt == nil {
		if err := s.projector.Project(ctx, record.Request); err != nil {
			return SelfDeletionView{}, ErrSelfDeletionUnavailable
		}
		confirmed, confirmErr := s.store.SelfDeletionByReceiptLookup(ctx, record.ReceiptLookupHash)
		if confirmErr != nil || confirmed.Request.PlatformBlockedAt == nil {
			return SelfDeletionView{}, ErrSelfDeletionUnavailable
		}
		record = confirmed
	}
	record, err = s.store.RetrySelfDeletion(ctx, record.ReceiptLookupHash, s.now().UTC())
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return SelfDeletionView{}, ErrSelfDeletionReceiptNotFound
		}
		return SelfDeletionView{}, err
	}
	return selfDeletionView(record), nil
}

func (s *SelfDeletion) recordForReceipt(ctx context.Context, receipt string) (SelfDeletionRecord, error) {
	if !validReceipt(receipt) {
		return SelfDeletionRecord{}, ErrSelfDeletionReceiptNotFound
	}
	lookupHash, receiptHash := receiptDigests(receipt)
	record, err := s.store.SelfDeletionByReceiptLookup(ctx, lookupHash)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return SelfDeletionRecord{}, ErrSelfDeletionReceiptNotFound
		}
		return SelfDeletionRecord{}, err
	}
	now := s.now().UTC()
	if !validSelfDeletionRecord(record, receiptHash, now) {
		return SelfDeletionRecord{}, ErrSelfDeletionReceiptNotFound
	}
	return record, nil
}

func validSelfDeletionRecord(record SelfDeletionRecord, receiptHash [sha256.Size]byte, now time.Time) bool {
	return subtle.ConstantTimeCompare(record.ReceiptHash[:], receiptHash[:]) == 1 &&
		record.ReceiptRevokedAt == nil && now.Before(record.ReceiptExpiresAt)
}

func selfDeletionView(record SelfDeletionRecord) SelfDeletionView {
	status := SelfDeletionBlocking
	if record.Request.PlatformBlockedAt != nil {
		switch record.Request.Status {
		case user.DeletionRequestPending:
			status = SelfDeletionPending
		case user.DeletionRequestProcessing:
			status = SelfDeletionProcessing
		case user.DeletionRequestCompleted:
			status = SelfDeletionCompleted
		case user.DeletionRequestManualIntervention:
			status = SelfDeletionManualIntervention
		}
	}
	return SelfDeletionView{
		Status: status, Partial: record.HasCompletedStep && status != SelfDeletionCompleted,
		PlatformBlocked: record.Request.PlatformBlockedAt != nil,
		RequestedAt:     record.Request.CreatedAt, UpdatedAt: record.Request.UpdatedAt,
		CompletedAt: record.Request.CompletedAt, ReceiptExpiresAt: record.ReceiptExpiresAt,
	}
}

func validIdempotencyKey(value string) bool {
	if len(value) != 43 || strings.Contains(value, "=") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 32
}

func validReceipt(value string) bool {
	return strings.HasPrefix(value, "adr_") && validIdempotencyKey(strings.TrimPrefix(value, "adr_"))
}

func scopedIdempotencyHash(subjectID uuid.UUID, key string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte("account-deletion-idempotency:v1\x00"))
	h.Write([]byte(subjectID.String()))
	h.Write([]byte{0})
	h.Write([]byte(key))
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func (s *SelfDeletion) receipt(subjectID uuid.UUID, idempotencyHash [sha256.Size]byte) string {
	h := hmac.New(sha256.New, s.config.ReceiptKey)
	h.Write([]byte("account-deletion-status-receipt:v1\x00"))
	h.Write([]byte(subjectID.String()))
	h.Write([]byte{0})
	h.Write(idempotencyHash[:])
	return "adr_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func receiptDigests(receipt string) ([sha256.Size]byte, [sha256.Size]byte) {
	lookup := sha256.Sum256(append([]byte("account-deletion-receipt-lookup:v1\x00"), []byte(receipt)...))
	proof := sha256.Sum256(append([]byte("account-deletion-receipt-proof:v1\x00"), []byte(receipt)...))
	return lookup, proof
}
