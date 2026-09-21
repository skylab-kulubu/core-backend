package account_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type recordingProjector struct {
	err      error
	requests []user.DeletionRequest
	store    interface {
		MarkDeletionPlatformBlocked(context.Context, uuid.UUID, time.Time) error
	}
	now    time.Time
	noMark bool
	after  func(user.DeletionRequest)
}

func (p *recordingProjector) Project(ctx context.Context, request user.DeletionRequest) error {
	p.requests = append(p.requests, request)
	if p.err != nil {
		return p.err
	}
	if p.noMark {
		return nil
	}
	if err := p.store.MarkDeletionPlatformBlocked(ctx, request.ID, p.now); err != nil {
		return err
	}
	if p.after != nil {
		p.after(request)
	}
	return nil
}

func selfDeleteFixture(t *testing.T) (*account.SelfDeletion, *user.MemoryStore, *recordingProjector, uuid.UUID, string) {
	t.Helper()
	store := user.NewMemoryStore()
	subject := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now().UTC().Truncate(time.Second)
	projector := &recordingProjector{store: store, now: now}
	service, err := account.NewSelfDeletion(store, projector, account.SelfDeletionConfig{
		Enabled:    true,
		ReceiptKey: []byte("0123456789abcdef0123456789abcdef"),
		ReceiptTTL: 90 * 24 * time.Hour,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return service, store, projector, subject, key
}

func TestSelfDeletionBeginIsSubjectScopedIdempotentAndRequiresProjection(t *testing.T) {
	t.Parallel()
	service, store, projector, subject, key := selfDeleteFixture(t)

	first, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != account.SelfDeletionPending || !first.PlatformBlocked || first.Partial {
		t.Fatalf("first status = %+v", first)
	}
	expectedNow := projector.now
	if !first.RequestedAt.Equal(expectedNow) || !first.UpdatedAt.Equal(expectedNow) ||
		!first.ReceiptExpiresAt.Equal(expectedNow.Add(90*24*time.Hour)) {
		t.Fatalf("self-delete chronology = %+v", first)
	}
	if len(first.Receipt) != len("adr_")+43 || first.Receipt[:4] != "adr_" {
		t.Fatalf("receipt shape = %q", first.Receipt)
	}
	if len(projector.requests) != 1 || projector.requests[0].SubjectID != subject {
		t.Fatalf("projected = %+v", projector.requests)
	}

	second, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}
	if second.Receipt != first.Receipt || second.RequestedAt != first.RequestedAt {
		t.Fatalf("idempotent response changed: first=%+v second=%+v", first, second)
	}
	request, err := store.DeletionRequest(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != projector.requests[0].ID || len(projector.requests) != 2 {
		t.Fatalf("durable request was not reused: request=%+v projections=%d", request, len(projector.requests))
	}

	otherKey := base64.RawURLEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))
	if _, err := service.Begin(context.Background(), subject, otherKey); !errors.Is(err, account.ErrSelfDeletionIdempotencyConflict) {
		t.Fatalf("different key error = %v", err)
	}
	otherSubject := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	otherSubjectResult, err := service.Begin(context.Background(), otherSubject, key)
	if err != nil {
		t.Fatal(err)
	}
	if otherSubjectResult.Receipt == first.Receipt {
		t.Fatal("same idempotency key was not scoped to the verified subject")
	}
}

func TestSelfDeletionBeginNeverSucceedsBeforeGlobalBlockConfirmation(t *testing.T) {
	t.Parallel()
	service, store, projector, subject, key := selfDeleteFixture(t)
	projector.err = errors.New("redis unavailable")

	if _, err := service.Begin(context.Background(), subject, key); !errors.Is(err, account.ErrSelfDeletionUnavailable) {
		t.Fatalf("projection error = %v", err)
	}
	request, err := store.DeletionRequest(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if request.PlatformBlockedAt != nil {
		t.Fatalf("failed projection confirmed block: %+v", request)
	}

	projector.err = nil
	retried, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.PlatformBlocked {
		t.Fatalf("retry did not confirm block: %+v", retried)
	}
}

func TestSelfDeletionBeginRejectsNoopProjectionWithoutDurableConfirmation(t *testing.T) {
	t.Parallel()
	service, _, projector, subject, key := selfDeleteFixture(t)
	projector.noMark = true

	if _, err := service.Begin(context.Background(), subject, key); !errors.Is(err, account.ErrSelfDeletionUnavailable) {
		t.Fatalf("no-op projection error = %v", err)
	}
}

func TestSelfDeletionBeginDoesNotDiscloseReceiptRevokedDuringProjection(t *testing.T) {
	t.Parallel()
	service, store, projector, subject, key := selfDeleteFixture(t)
	projector.after = func(request user.DeletionRequest) {
		if err := store.RevokeSelfDeletionReceipt(context.Background(), request.ID, projector.now); err != nil {
			t.Errorf("revoke receipt: %v", err)
		}
	}

	if _, err := service.Begin(context.Background(), subject, key); !errors.Is(err, account.ErrSelfDeletionUnavailable) {
		t.Fatalf("revoked-in-flight receipt error = %v", err)
	}
}

func TestSelfDeletionReceiptReadsPIIFreeStatusAndRetriesWithoutReactivation(t *testing.T) {
	t.Parallel()
	service, store, _, subject, key := selfDeleteFixture(t)
	started, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.Status(context.Background(), started.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if status.Receipt != "" || status.Status != account.SelfDeletionPending || status.Partial {
		t.Fatalf("public status = %+v", status)
	}

	request, ok, err := store.ClaimDeletionRequest(context.Background(), time.Now().Add(24*time.Hour), time.Minute)
	if err != nil || !ok || request.LeaseToken == nil {
		t.Fatalf("claim = %+v ok=%v err=%v", request, ok, err)
	}
	if err := store.CompleteDeletionStep(context.Background(), request.ID, *request.LeaseToken, user.DeletionStepDisableIdentity, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RetryDeletionRequest(context.Background(), request.ID, *request.LeaseToken, time.Now(), "disable_identity_failed", true, false); err != nil {
		t.Fatal(err)
	}

	manual, err := service.Status(context.Background(), started.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Status != account.SelfDeletionManualIntervention || !manual.Partial {
		t.Fatalf("manual status = %+v", manual)
	}
	retried, err := service.Retry(context.Background(), started.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != account.SelfDeletionPending || !retried.Partial {
		t.Fatalf("retried status = %+v", retried)
	}
	u, err := store.Get(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if u.AccountState != user.AccountDeletionPending {
		t.Fatalf("retry reactivated account: %q", u.AccountState)
	}
}

func TestSelfDeletionRejectsDisabledInvalidAndExpiredCapabilities(t *testing.T) {
	t.Parallel()
	service, store, projector, subject, key := selfDeleteFixture(t)
	if _, err := service.Begin(context.Background(), subject, "short"); !errors.Is(err, account.ErrSelfDeletionInvalidIdempotencyKey) {
		t.Fatalf("invalid key error = %v", err)
	}

	disabled, err := account.NewSelfDeletion(store, projector, account.SelfDeletionConfig{Enabled: false}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.Begin(context.Background(), subject, key); !errors.Is(err, account.ErrSelfDeletionDisabled) {
		t.Fatalf("disabled error = %v", err)
	}
	if _, err := service.Status(context.Background(), "adr_not-a-capability"); !errors.Is(err, account.ErrSelfDeletionReceiptNotFound) {
		t.Fatalf("invalid receipt error = %v", err)
	}

	started, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}
	request, err := store.DeletionRequest(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSelfDeletionReceipt(context.Background(), request.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Status(context.Background(), started.Receipt); !errors.Is(err, account.ErrSelfDeletionReceiptNotFound) {
		t.Fatalf("revoked receipt error = %v", err)
	}
}

func TestSelfDeletionReceiptExpiresAtItsFixedBound(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	subject := uuid.New()
	key := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	now := time.Now().UTC().Truncate(time.Second)
	projector := &recordingProjector{store: store, now: now}
	service, err := account.NewSelfDeletion(store, projector, account.SelfDeletionConfig{
		Enabled: true, ReceiptKey: []byte("0123456789abcdef0123456789abcdef"), ReceiptTTL: time.Hour,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Begin(context.Background(), subject, key)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := service.Status(context.Background(), started.Receipt); !errors.Is(err, account.ErrSelfDeletionReceiptNotFound) {
		t.Fatalf("expired receipt error = %v", err)
	}
}
