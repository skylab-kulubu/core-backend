package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

// YTUAttributes are the `university` and `department` attributes of one
// Keycloak account, as the one-time backfill reads them.
type YTUAttributes struct {
	ID         uuid.UUID
	University string
	Department string
}

// YTUBackfillReport counts what BackfillYTU did (or, on a dry run, would do).
// It names no person.
type YTUBackfillReport struct {
	// Accounts carrying the YTÜ university.
	Accounts int
	// Updated records, written only when a value differs.
	Updated int
	// Unchanged records, already in step.
	Unchanged int
	// NoRecord accounts have no core record yet; their first request makes one.
	NoRecord int
	// Blocked records are pending deletion or anonymized and are left alone.
	Blocked int
	// Unresolved counts the raw department values that gave no faculty, so
	// the table can be extended.
	Unresolved map[string]int
}

// BackfillYTU brings existing records in step with the Keycloak attributes
// the YTÜ Microsoft login wrote, for people who do not sign in again. It uses
// the same rule and comparison as the login, so running it twice changes
// nothing the second time. With apply false it only counts.
func BackfillYTU(ctx context.Context, store Store, accounts []YTUAttributes, apply bool) (YTUBackfillReport, error) {
	report := YTUBackfillReport{Unresolved: map[string]int{}}
	for _, account := range accounts {
		p, linked := ytu.FromClaims(account.University, account.Department)
		if !linked {
			continue
		}
		report.Accounts++
		if p.Faculty == "" {
			report.Unresolved[account.Department]++
		}
		existing, err := store.Get(ctx, account.ID)
		if errors.Is(err, ErrNotFound) {
			report.NoRecord++
			continue
		}
		if err != nil {
			return report, fmt.Errorf("read core record: %w", err)
		}
		switch {
		case existing.AccountState != AccountActive:
			report.Blocked++
		case !ytuDiffers(existing, p):
			report.Unchanged++
		case !apply:
			report.Updated++
		default:
			if _, err := store.SetYTUProfile(ctx, account.ID, p); errors.Is(err, ErrAccountBlocked) {
				report.Blocked++
				continue
			} else if err != nil {
				return report, fmt.Errorf("write core record: %w", err)
			}
			report.Updated++
		}
	}
	return report, nil
}
