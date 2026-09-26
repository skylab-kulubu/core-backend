package media

import (
	"context"
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
	// (certificate template assets). They stay legacy until private Media
	// storage moves their blobs.
	KeptPrivate int
	// KeptMixed legacy Media have uses that no one purpose fits, such as an
	// Event cover that is also someone's profile picture. They stay legacy.
	KeptMixed int
	Failed    int
}

// BackfillLegacyPurposes makes one pass over the legacy Media that core
// attaches and gives each one the Media purpose of its use.
func BackfillLegacyPurposes(ctx context.Context, store *PostgresStore, catalogue Catalogue, onError func(error)) (LegacyPurposeReport, error) {
	var report LegacyPurposeReport
	pass, err := BackfillPass(ctx, "media", store.listLegacyAttachedByCore,
		func(id uuid.UUID) uuid.UUID { return id },
		func(ctx context.Context, id uuid.UUID) error {
			var kept legacyKeep
			assigned, err := store.assignLegacyPurpose(ctx, id, func(uses []attachmentUse) string {
				var purpose string
				purpose, kept = legacyPurpose(uses, catalogue)
				return purpose
			})
			if err != nil {
				return err
			}
			switch {
			case assigned != "":
				report.Assigned++
			case kept == keptPrivate:
				report.KeptPrivate++
			case kept == keptMixed:
				report.KeptMixed++
			}
			return nil
		},
		onError)
	report.Failed = pass.Failed
	return report, err
}

// MaintainLegacyPurposeBackfill runs BackfillLegacyPurposes in the
// background, off the request path, until a pass leaves nothing failed,
// reporting each pass that finishes through onPass.
func MaintainLegacyPurposeBackfill(ctx context.Context, store *PostgresStore, catalogue Catalogue, retryEvery time.Duration, onPass func(LegacyPurposeReport), onError func(error)) {
	MaintainBackfill(ctx, func(ctx context.Context) (BackfillReport, error) {
		report, err := BackfillLegacyPurposes(ctx, store, catalogue, onError)
		if err == nil && onPass != nil {
			onPass(report)
		}
		return BackfillReport{Applied: report.Assigned, Failed: report.Failed}, err
	}, retryEvery, onError)
}

// legacyKeep is why the backfill keeps a legacy Media legacy.
type legacyKeep int

const (
	notKept legacyKeep = iota
	keptPrivate
	keptMixed
)

// legacyUses are core's roles, each with the Media purpose of that use, in
// the order the backfill tries them.
var legacyUses = []struct {
	role    Role
	purpose string
}{
	{RoleEventCover, PurposeEventCover},
	{RoleEventGallery, PurposeEventGallery},
	{RoleProfilePicture, PurposeProfilePicture},
	{RoleCertificateAsset, PurposeCertificateAsset},
}

// legacyPurpose is the Media purpose a legacy Media gets from its uses, or
// "" and why it stays legacy. The purpose is the one of the first of core's
// roles the Media plays whose purpose fits every one of its uses, as a new
// link would be checked (fits): both Event purposes fit both Event roles, so
// a cover that is also a gallery photo takes event_cover. A private purpose
// is never given: the Media's blob is public.
func legacyPurpose(uses []attachmentUse, catalogue Catalogue) (string, legacyKeep) {
	played := false
	for _, candidate := range legacyUses {
		if !slices.Contains(uses, attachmentUse{Product: authz.ProductCore, Role: candidate.role}) {
			continue
		}
		played = true
		fitsEvery := !slices.ContainsFunc(uses, func(use attachmentUse) bool {
			return !fits(use.Product, use.Role, candidate.purpose)
		})
		if !fitsEvery {
			continue
		}
		if purpose, ok := catalogue.Lookup(candidate.purpose); !ok || purpose.Visibility == VisibilityPrivate {
			return "", keptPrivate
		}
		return candidate.purpose, notKept
	}
	if !played {
		// Core stopped using it since it was listed.
		return "", notKept
	}
	return "", keptMixed
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
	return LegacyReport{Orphans: orphans, CoreLinksWithoutAttachment: unattached, AttachedByCore: attached}, nil
}

// LegacyOrphanWindow is how long a legacy orphan is kept once Yusuf has
// reviewed the report and started its window (Q19): the same 30 days as a
// detached Media.
const LegacyOrphanWindow = 30 * 24 * time.Hour

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
	at := now.Add(LegacyOrphanWindow)
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
