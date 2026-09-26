package media

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrProfileBlobNotErased = errors.New("media: profile blob erasure not satisfied")

var ErrStagedUploadNotErased = errors.New("media: staged upload erasure not satisfied")

// ErrPersonalMediaNotErased is a Media account erasure recorded as the
// person's own that is not purged yet: its object could not be deleted, or
// another purge holds it. The worker retries the step.
var ErrPersonalMediaNotErased = errors.New("media: recorded personal Media erasure not satisfied")

// errObjectDelete stands for a failed object deletion in an erasure error.
// The storage client's error can name the object's address, which an error
// the erasure worker logs must not carry; errors.Is still reaches the cause.
type errObjectDelete struct{ cause error }

func (e errObjectDelete) Error() string { return "media: object storage delete failed" }
func (e errObjectDelete) Unwrap() error { return e.cause }

// erasureTexts are the errors an erasure error may name: core wrote their
// text, and it holds no Media, object, file or person.
var erasureTexts = []error{
	ErrPersonalMediaNotErased, ErrProfileBlobNotErased, ErrPrivateMediaDisabled, ErrNotFound,
	context.DeadlineExceeded, context.Canceled,
}

// erasureError is an erasure error as the erasure worker may log it
// (account-lifecycle.md): the text of the erasureTexts and object deletion
// failures it holds, the SQLSTATE of a database error, or only that the
// erasure failed. Anything
// else can name a Media: a database message, a record core cannot read.
// errors.Is and errors.As still reach the cause.
func erasureError(err error) error {
	if err == nil {
		return nil
	}
	var texts []string
	for _, known := range erasureTexts {
		if errors.Is(err, known) {
			texts = append(texts, known.Error())
		}
	}
	if errors.As(err, new(errObjectDelete)) {
		texts = append(texts, errObjectDelete{}.Error())
	}
	var database *pgconn.PgError
	if errors.As(err, &database) {
		texts = append(texts, "media: database error (SQLSTATE "+database.Code+")")
	}
	if len(texts) == 0 {
		texts = append(texts, "media: erasure failed")
	}
	return redactedError{text: strings.Join(texts, ": "), cause: err}
}

type redactedError struct {
	text  string
	cause error
}

func (e redactedError) Error() string { return e.text }
func (e redactedError) Unwrap() error { return e.cause }

// erasedPersonalPurposes are the Media purposes of a person's own files
// (media redesign spec, Account erasure): their account erasure purges them
// at once. Every other purpose (event_cover, event_gallery, cms_image, cms_file,
// club_file, video, certificate_asset) is club content, which keeps its file
// without the uploader and the file name.
var erasedPersonalPurposes = []string{PurposeProfilePicture, PurposeAnswerFile, PurposeAnswerFileLarge}

// legacyIsPersonalSQL is the account erasure's rule for a legacy Media, as
// an SQL condition on the Media id names: decision E1 (Yusuf, 2026-09-27).
// A legacy Media nothing uses (no Media attachment and none of core's own
// links, the safety net) is the person's own and purged at once; one
// anything still uses is club content, kept without its uploader and name.
// This is the only place the rule lives.
func legacyIsPersonalSQL(id string) string {
	return `NOT ` + referencedSQL(id)
}

// RecordAccountErasure is anonymize_core's part in the person's uploads. It
// runs in the caller's transaction, which already holds core's link tables
// and the person's user row, before the caller clears their uploader
// (UPDATE media SET uploaded_by = NULL). In this order it
//
//  1. locks the person's uploads, so no Media attachment is written to one
//     while it is being told apart;
//  2. records in account_deletion_media, by id only, the uploads
//     erase_profile_media purges at once: every upload of a personal purpose
//     and every legacy upload legacyIsPersonalSQL makes personal. The current
//     profile picture (profilePicture) is never recorded: the profile-picture
//     erasure (profile_media_id) has it, and keeps it when club content also
//     uses it;
//  3. clears the file name of every upload of theirs: the club content kept,
//     and the personal Media too, whose records stay after their objects go.
//
// A rerun records nothing new and changes no existing record: once the
// uploader is cleared nothing is found, and a record already there is kept
// (ON CONFLICT DO NOTHING).
func RecordAccountErasure(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, profilePicture *uuid.UUID, at time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM media WHERE uploaded_by = $1 ORDER BY id FOR UPDATE`, subjectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_deletion_media (request_id, media_id)
		SELECT request.id, upload.id
		FROM account_deletion_requests request
		JOIN media upload ON upload.uploaded_by = request.subject_id
		WHERE request.subject_id = $1
		  AND upload.id IS DISTINCT FROM $2
		  AND upload.blob_purged_at IS NULL
		  AND (upload.purpose = ANY ($3) OR (upload.purpose = $4 AND `+legacyIsPersonalSQL("upload.id")+`))
		ON CONFLICT (request_id, media_id) DO NOTHING
	`, subjectID, profilePicture, erasedPersonalPurposes, PurposeLegacy); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE media SET file_name = '', updated_at = $2 WHERE uploaded_by = $1 AND file_name <> ''`, subjectID, at)
	return err
}

// ImmediateBlobEraser bypasses the ordinary recovery window only for the
// Media the irreversible account-erasure workflow selects. blobs is
// Buckets, so an object is deleted from the bucket that holds it, and a
// missing object counts as deleted.
type ImmediateBlobEraser struct {
	media *PostgresStore
	blobs BlobStore
}

func NewImmediateBlobEraser(media *PostgresStore, blobs BlobStore) *ImmediateBlobEraser {
	return &ImmediateBlobEraser{media: media, blobs: blobs}
}

// EnsureErased erases a Media erase_profile_media names: a Media
// anonymize_core recorded as the person's own, or their profile picture.
//
// A recorded Media is purged whatever still uses it, an Answer file a
// Skyforms response still holds included: personal purposes win. Its record
// goes in the transaction that purges it. The profile picture is purged only
// once nothing uses it (the store's locked reference check), so a picture an
// Event or a certificate also uses keeps its object and loses only its
// uploader and name.
//
// The error goes to the erasure worker's log, so it names no Media, object,
// file or person (erasureError).
func (e *ImmediateBlobEraser) EnsureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	return erasureError(e.ensureErased(ctx, id, at))
}

func (e *ImmediateBlobEraser) ensureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	recorded, err := e.media.recordedForErasure(ctx, id)
	if err != nil {
		return err
	}
	if recorded {
		return e.erasePersonal(ctx, id, at)
	}
	purged, err := e.media.PurgeBlobIfUnreferenced(ctx, id, at, e.deleteObject(ctx))
	if err != nil || purged {
		return err
	}
	item, getErr := e.media.GetIncludingDeleted(ctx, id)
	if getErr != nil {
		return getErr
	}
	if item.BlobPurgedAt != nil {
		return nil
	}
	// false,nil also means current/restored or newly referenced. Neither is a
	// completed erase for a deletion candidate, so retain the request linkage
	// and let the worker retry or surface manual intervention.
	return ErrProfileBlobNotErased
}

// erasePersonal purges a recorded Media through the store's two-phase purge
// (erasedQueue), with its locks and its durable claim, but without the
// reference check of a personal purpose. A legacy Media something uses by
// now is kept instead, its record gone. A Media already purged another way
// only loses its record.
func (e *ImmediateBlobEraser) erasePersonal(ctx context.Context, id uuid.UUID, at time.Time) error {
	purged, err := e.media.purgeBlob(ctx, id, at, erasedQueue, e.deleteObject(ctx))
	if err != nil {
		return errors.Join(ErrPersonalMediaNotErased, err)
	}
	if purged {
		return nil
	}
	item, err := e.media.GetIncludingDeleted(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err != nil || item.BlobPurgedAt != nil {
		return e.media.forgetErased(ctx, id)
	}
	recorded, err := e.media.recordedForErasure(ctx, id)
	if err != nil || !recorded {
		// Not recorded any more: the purge kept it as club content.
		return err
	}
	// Another purge holds the claim (or reset it): try again later.
	return ErrPersonalMediaNotErased
}

func (e *ImmediateBlobEraser) deleteObject(ctx context.Context) func(string) error {
	return func(key string) error {
		err := e.blobs.Delete(ctx, key)
		if err == nil || errors.Is(err, ErrPrivateMediaDisabled) {
			return err
		}
		return errObjectDelete{cause: err}
	}
}

// recordedForErasure reports whether account erasure recorded the Media as
// a person's own and has not purged it yet.
func (s *PostgresStore) recordedForErasure(ctx context.Context, id uuid.UUID) (bool, error) {
	var recorded bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM account_deletion_media WHERE media_id = $1)`, id).Scan(&recorded)
	return recorded, err
}

// forgetErased removes account erasure's record of a Media that is already
// purged.
func (s *PostgresStore) forgetErased(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM account_deletion_media WHERE media_id = $1`, id)
	return err
}

func (e *ImmediateBlobEraser) EnsureSubjectUploadsErased(ctx context.Context, subjectID uuid.UUID, at time.Time) error {
	for {
		found, err := e.media.PurgeNextSubjectStagedUpload(ctx, subjectID, at, func(key string) error {
			return e.blobs.Delete(ctx, key)
		})
		if err != nil {
			return errors.Join(ErrStagedUploadNotErased, err)
		}
		if !found {
			return nil
		}
	}
}
