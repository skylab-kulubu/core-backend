package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// attachmentUse is what one Media attachment makes of a Media: the product
// whose record links it and the role it plays there.
type attachmentUse struct {
	Product authz.Product
	Role    Role
}

// LegacyPurposeReport counts one pass of the legacy purpose backfill.
type LegacyPurposeReport struct {
	// Assigned legacy Media got the purpose of their use.
	Assigned int
	// KeptPrivate legacy Media are used where only a private purpose fits
	// (certificate template assets). They stay legacy for good: nothing
	// moves their public blobs into private storage (decision G1).
	KeptPrivate int
	// KeptMixed legacy Media have uses that no one purpose fits, such as an
	// Event cover that is also someone's profile picture (decision K1).
	// They stay legacy.
	KeptMixed int
	// Skipped Media were no longer legacy, or no longer used by core, when
	// the backfill reached them.
	Skipped int
	Failed  int
}

// BackfillLegacyPurposes makes one pass over the legacy Media that core
// attaches and gives each one the Media purpose of its use, holding its
// detach expiry (decision K2, see ReleaseDetachExpiryHold).
func BackfillLegacyPurposes(ctx context.Context, store *PostgresStore, catalogue Catalogue, onError func(error)) (LegacyPurposeReport, error) {
	var report LegacyPurposeReport
	pass, err := BackfillPass(ctx, "media", store.listLegacyAttachedByCore,
		func(id uuid.UUID) uuid.UUID { return id },
		func(ctx context.Context, id uuid.UUID) error {
			decision, err := store.assignLegacyPurpose(ctx, id, func(uses []attachmentUse) (string, legacyDecision, error) {
				return legacyPurpose(uses, catalogue)
			})
			if err != nil {
				return err
			}
			switch decision {
			case legacyAssigned:
				report.Assigned++
			case legacyKeptPrivate:
				report.KeptPrivate++
			case legacyKeptMixed:
				report.KeptMixed++
			default:
				report.Skipped++
			}
			return nil
		},
		onError)
	report.Failed = pass.Failed
	return report, err
}

// MaintainLegacyPurposeBackfill runs BackfillLegacyPurposes in the
// background, off the request path, until a pass leaves nothing failed. A
// pass is reported through onPass when it assigned a purpose or failed on a
// different number of Media than the pass before, and a Media that keeps
// failing is reported through onError at most once an hour. onAssigned is
// called after every pass that assigned a purpose, finished or not: the
// image size backfill's rerun, so those Media get their sizes.
func MaintainLegacyPurposeBackfill(ctx context.Context, store *PostgresStore, catalogue Catalogue, retryEvery time.Duration, onAssigned func(), onPass func(LegacyPurposeReport), onError func(error)) {
	itemErrors := throttleItemErrors(onError, time.Hour, time.Now)
	lastFailed := 0
	MaintainBackfill(ctx, func(ctx context.Context) (BackfillReport, error) {
		report, err := BackfillLegacyPurposes(ctx, store, catalogue, itemErrors)
		if report.Assigned > 0 && onAssigned != nil {
			onAssigned()
		}
		if err == nil && onPass != nil && (report.Assigned > 0 || report.Failed != lastFailed) {
			onPass(report)
		}
		lastFailed = report.Failed
		return BackfillReport{Applied: report.Assigned, Failed: report.Failed}, err
	}, retryEvery, onError)
}

// throttleItemErrors passes on an item's failure (a *BackfillError) the first
// time and then at most once every interval; other errors always.
func throttleItemErrors(onError func(error), every time.Duration, now func() time.Time) func(error) {
	if onError == nil {
		return nil
	}
	reported := map[uuid.UUID]time.Time{}
	return func(err error) {
		var item *BackfillError
		if errors.As(err, &item) {
			if at, seen := reported[item.ID]; seen && now().Sub(at) < every {
				return
			}
			reported[item.ID] = now()
		}
		onError(err)
	}
}

// legacyDecision is what the backfill did with one legacy Media.
type legacyDecision int

const (
	// legacySkipped: no longer legacy, or no longer used by core.
	legacySkipped legacyDecision = iota
	legacyAssigned
	legacyKeptPrivate
	legacyKeptMixed
)

// legacyRoles are core's roles in the order the backfill tries their
// purposes. A role's own purpose is the first rolePurposes lists for it.
var legacyRoles = []Role{RoleEventCover, RoleEventGallery, RoleProfilePicture, RoleCertificateAsset}

// legacyPurpose decides what a legacy Media used as uses becomes: the own
// purpose of the first of core's roles it plays that fits every one of its
// uses, as a new link would be checked (fits). Both Event purposes fit both
// Event roles, so a cover that is also a gallery photo takes event_cover. A
// private purpose is never given: the Media's blob is public.
func legacyPurpose(uses []attachmentUse, catalogue Catalogue) (string, legacyDecision, error) {
	played := false
	for _, role := range legacyRoles {
		if !slices.Contains(uses, attachmentUse{Product: authz.ProductCore, Role: role}) {
			continue
		}
		played = true
		candidate := rolePurposes[authz.ProductCore][role][0]
		if slices.ContainsFunc(uses, func(use attachmentUse) bool { return !fits(use.Product, use.Role, candidate) }) {
			continue
		}
		purpose, ok := catalogue.Lookup(candidate)
		if !ok {
			return "", legacySkipped, fmt.Errorf("purpose %s is not in the catalogue", candidate)
		}
		if purpose.Visibility == VisibilityPrivate {
			return "", legacyKeptPrivate, nil
		}
		return candidate, legacyAssigned, nil
	}
	if !played {
		return "", legacySkipped, nil
	}
	return "", legacyKeptMixed, nil
}

// LegacyReport is what Yusuf reviews before any legacy Media is removed
// (media redesign Q19).
type LegacyReport struct {
	// Orphans are the current legacy Media nothing in core uses, oldest
	// upload first. Skyforms answers and CMS content use Media by address,
	// which core cannot see: an orphan may still be used there.
	Orphans []Media
	// CoreLinksWithoutAttachment counts core's own links (an Event cover or
	// gallery photo, a profile picture, a certificate template asset) whose
	// Media attachment is missing. While it is not zero, the purge and this
	// report keep reading core's links directly beside the Media
	// attachments.
	CoreLinksWithoutAttachment int
	// AttachedByCore counts the legacy Media core still attaches: what the
	// purpose backfill has not reached yet, and what it keeps legacy
	// (certificate template assets, mixed uses).
	AttachedByCore int
	// DetachExpiryHeld counts the Media the backfill gave a purpose whose
	// detach expiry is still held (decision K2, ReleaseDetachExpiryHold).
	DetachExpiryHeld int
	// HoldReleasedAt is when the hold was released (ticket 18); nil before.
	// After it the backfill gives purposes without the hold.
	HoldReleasedAt *time.Time
}

// ReportLegacy reads the legacy report. It changes nothing.
func ReportLegacy(ctx context.Context, store *PostgresStore) (LegacyReport, error) {
	orphans, err := store.listLegacyOrphans(ctx)
	if err != nil {
		return LegacyReport{}, err
	}
	unattached, err := store.countCoreLinksWithoutAttachment(ctx)
	if err != nil {
		return LegacyReport{}, err
	}
	attached, err := store.countLegacyAttachedByCore(ctx)
	if err != nil {
		return LegacyReport{}, err
	}
	held, _, err := store.countDetachExpiryHeld(ctx)
	if err != nil {
		return LegacyReport{}, err
	}
	released, err := store.holdReleasedAt(ctx)
	if err != nil {
		return LegacyReport{}, err
	}
	return LegacyReport{Orphans: orphans, CoreLinksWithoutAttachment: unattached, AttachedByCore: attached, DetachExpiryHeld: held, HoldReleasedAt: released}, nil
}

// DetachedWindow is how long a Media no record uses any more is kept: the
// 30 days the database gives a detached Media. A reviewed legacy orphan
// (Q19) and a Media detached while its expiry was held (K2) get the same.
const DetachedWindow = 30 * 24 * time.Hour

// LegacyExpiryReport counts one run of ExpireLegacyOrphans.
type LegacyExpiryReport struct {
	// Expiring orphans got their window (with apply) or would get it.
	Expiring int
	// AlreadyExpiring orphans had a window already; it is not restarted.
	AlreadyExpiring int
	// NotOrphans are the ids that are not legacy orphans (any more): used by
	// a record, given a purpose, archived, purged, or unknown. They are left
	// alone.
	NotOrphans []uuid.UUID
	Failed     int
}

// ExpireLegacyOrphans starts the 30-day window of each listed Media that is
// still a legacy orphan, from now; the expiry cleanup purges it when the
// window ends unless something attaches it first. Without apply it only
// counts. Each Media is checked again as it is written, so one used since
// the report is left alone. A Media that fails is reported through onError
// and does not stop the others.
func ExpireLegacyOrphans(ctx context.Context, store *PostgresStore, ids []uuid.UUID, now time.Time, apply bool, onError func(error)) (LegacyExpiryReport, error) {
	report := LegacyExpiryReport{NotOrphans: []uuid.UUID{}}
	at := now.Add(DetachedWindow)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		state, err := store.expireLegacyOrphan(ctx, id, at, apply)
		if err != nil {
			report.Failed++
			if onError != nil {
				onError(fmt.Errorf("media %s: %w", id, err))
			}
			continue
		}
		switch state {
		case orphanExpiring:
			report.Expiring++
		case orphanAlreadyExpiring:
			report.AlreadyExpiring++
		default:
			report.NotOrphans = append(report.NotOrphans, id)
		}
	}
	return report, nil
}

// HoldReleaseReport counts one run of ReleaseDetachExpiryHold.
type HoldReleaseReport struct {
	// Held Media had their detach expiry held when the run began.
	Held int
	// HeldDetached of them were current and used by no record: releasing
	// starts their window.
	HeldDetached int
	// Released Media follow their purpose again.
	Released int
	// WindowsStarted of them got their 30 days from the release.
	WindowsStarted int
	Failed         int
}

// ReleaseDetachExpiryHold ends the hold the legacy purpose backfill put on
// the Media it gave a purpose (decision K2), once stage 5 has given the uses
// core could not see their Media attachments (ticket 18). The release is
// recorded, so the backfill gives purposes without the hold from then on
// (the first release's time is kept). Every held Media follows its purpose
// again; one that no record uses by then gets its 30
// days from now, not from when it was detached. Without apply it only
// counts. It walks the held Media by id in batches: a Media that fails is
// reported through onError and stays held for the next run, and a run over
// released Media changes nothing.
func ReleaseDetachExpiryHold(ctx context.Context, store *PostgresStore, now time.Time, apply bool, onError func(error)) (HoldReleaseReport, error) {
	var report HoldReleaseReport
	var err error
	if apply {
		// Recorded before anything is counted: the record waits for every
		// backfill that is writing a hold, and no backfill writes one
		// after it, so the count and the walk below see every hold.
		if err := store.recordHoldRelease(ctx, now); err != nil {
			return report, err
		}
	}
	report.Held, report.HeldDetached, err = store.countDetachExpiryHeld(ctx)
	if err != nil || !apply {
		return report, err
	}
	at := now.Add(DetachedWindow)
	pass, err := BackfillPass(ctx, "media", store.listDetachExpiryHeld,
		func(id uuid.UUID) uuid.UUID { return id },
		func(ctx context.Context, id uuid.UUID) error {
			released, windowStarted, err := store.releaseDetachExpiryHold(ctx, id, at)
			if err != nil {
				return err
			}
			if released {
				report.Released++
			}
			if windowStarted {
				report.WindowsStarted++
			}
			return nil
		},
		onError)
	report.Failed = pass.Failed
	return report, err
}
