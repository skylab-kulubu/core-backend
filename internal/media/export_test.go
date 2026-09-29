package media

import (
	"context"

	"github.com/google/uuid"
)

// Unexported functions the external tests reach.
var (
	RebuildICCTags    = rebuildICCTags
	GuardICC          = guardICC
	CoverColorsInSlot = coverColorsInSlot
	FaststartCopyKey  = faststartCopyKey
	VideoOriginalOf   = videoOriginalOf
	CopiesPrefix      = faststartCopiesPrefix
)

// BackfillOneLegacyPurpose runs the purpose backfill's step for one Media,
// with its real lock, read and write, and calls locked once the Media row is
// locked and its Media attachments read, before anything is written.
func BackfillOneLegacyPurpose(ctx context.Context, store *PostgresStore, catalogue Catalogue, id uuid.UUID, locked func()) error {
	_, err := store.assignLegacyPurpose(ctx, id, func(uses []attachmentUse) (string, legacyDecision, error) {
		locked()
		return legacyPurpose(uses, catalogue)
	})
	return err
}

// PartSizeFor is the part size the worker writes a copy of size bytes in.
func (w *FaststartWorker) PartSizeFor(size int64) int64 { return w.partSizeFor(size) }
