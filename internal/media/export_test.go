package media

import (
	"context"

	"github.com/google/uuid"
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
