package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

var ErrProfileBlobNotErased = errors.New("media: profile blob erasure not satisfied")

var ErrStagedUploadNotErased = errors.New("media: staged upload erasure not satisfied")

// ErrRecordedMediaNotErased is an upload anonymize_core recorded that
// erase_profile_media has not finished yet: its object could not be deleted
// or given new metadata, or another purge holds it. The worker retries the
// step.
var ErrRecordedMediaNotErased = errors.New("media: recorded Media erasure not satisfied")

// errObjectStorage stands for a failed object storage request in an erasure
// error. The storage client's error can name the object's address, which an
// error the erasure worker logs must not carry; errors.Is still reaches the
// cause.
type errObjectStorage struct {
	request string
	cause   error
}

func (e errObjectStorage) Error() string { return "media: object storage " + e.request + " failed" }
func (e errObjectStorage) Unwrap() error { return e.cause }

// erasureTexts are the errors an erasure error may name: core wrote their
// text, and it holds no Media, object, file or person.
var erasureTexts = []error{
	ErrRecordedMediaNotErased, ErrProfileBlobNotErased, ErrPrivateMediaDisabled, ErrNotFound,
	context.DeadlineExceeded, context.Canceled,
}

// erasureError is an erasure error as the erasure worker may log it
// (account-lifecycle.md): the text of the erasureTexts and object storage
// failures it holds, the SQLSTATE of a database error, or only that the
// erasure failed. Anything else can name a Media: a database message, a
// record core cannot read. errors.Is and errors.As still reach the cause.
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
	var storage errObjectStorage
	if errors.As(err, &storage) {
		texts = append(texts, storage.Error())
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

// personalOnErasureSQL is the account erasure's rule, the one place it
// lives: whether an upload the person's erasure recorded is theirs, and
// purged at once, or club content, which keeps its file without their name.
// It is an SQL condition on the Media id names, read under the purge's locks
// once the person's own profile link is gone:
//
//   - answer_file, answer_file_large: theirs;
//   - profile_picture: theirs, unless it is someone's current profile
//     picture, which makes it that person's;
//   - legacy (decision E1, Yusuf, 2026-09-27): theirs while only personal
//     uses hold it (a Skyforms answer); any other use (another product's
//     Media attachment, one of core's own links, the safety net, someone's
//     profile) makes it club content;
//   - every other purpose (event_cover, event_gallery, cms_image, cms_file,
//     club_file, video, certificate_asset): club content.
func personalOnErasureSQL(id string) string {
	return `(SELECT CASE upload.purpose
			WHEN '` + PurposeAnswerFile + `' THEN true
			WHEN '` + PurposeAnswerFileLarge + `' THEN true
			WHEN '` + PurposeProfilePicture + `' THEN NOT EXISTS (SELECT 1 FROM users WHERE profile_picture_id = upload.id)
			WHEN '` + PurposeLegacy + `' THEN NOT EXISTS (
				SELECT 1 FROM media_attachments link
				WHERE link.media_id = upload.id
				  AND NOT (link.owner_service = '` + string(authz.ProductForms) + `' AND link.role = '` + string(RoleFormsAnswer) + `')
				UNION ALL ` + coreLinksSQL("upload.id") + `
			)
			ELSE false
		END FROM media upload WHERE upload.id = ` + id + `)`
}

// recordedSQL is true while account erasure has the Media ($1) to erase.
const recordedSQL = `EXISTS (SELECT 1 FROM account_deletion_media WHERE media_id = $1)`

func recordedForErasure(ctx context.Context, db postgresMediaTx, id uuid.UUID) (bool, error) {
	var recorded bool
	err := db.QueryRow(ctx, `SELECT `+recordedSQL, id).Scan(&recorded)
	return recorded, err
}

// erasedAsPersonal reports, under the purge's locks, whether account erasure
// recorded the Media and it is the person's own (personalOnErasureSQL).
func erasedAsPersonal(ctx context.Context, tx postgresMediaTx, id uuid.UUID) (bool, error) {
	var personal bool
	err := tx.QueryRow(ctx, `SELECT `+recordedSQL+` AND COALESCE(`+personalOnErasureSQL("$1")+`, false)`, id).Scan(&personal)
	return personal, err
}

// forgetErased removes account erasure's record of a Media it is done with.
func forgetErased(ctx context.Context, db postgresMediaTx, id uuid.UUID) error {
	_, err := db.Exec(ctx, `DELETE FROM account_deletion_media WHERE media_id = $1`, id)
	return err
}

// RecordAccountErasure is anonymize_core's part in the person's uploads. It
// runs in the caller's transaction, which already holds core's link tables
// and the person's user row, before the caller clears their uploader
// (UPDATE media SET uploaded_by = NULL). In this order it
//
//  1. records in account_deletion_media, by id only, every upload of theirs
//     that still has its object, for erase_profile_media to purge or keep by
//     its purpose then (personalOnErasureSQL). The current profile picture
//     (profilePicture) is not recorded here: the profile-picture erasure
//     (profile_media_id) has it, or, when club content also uses it,
//     RecordSharedProfilePicture records it;
//  2. clears the file name of every upload of theirs.
//
// A rerun records nothing new and changes no existing record: once the
// uploader is cleared nothing is found, and a record already there is kept
// (ON CONFLICT DO NOTHING).
func RecordAccountErasure(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, profilePicture *uuid.UUID, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO account_deletion_media (request_id, media_id)
		SELECT request.id, upload.id
		FROM account_deletion_requests request
		JOIN media upload ON upload.uploaded_by = request.subject_id
		WHERE request.subject_id = $1
		  AND upload.id IS DISTINCT FROM $2
		  AND upload.blob_purged_at IS NULL
		ON CONFLICT (request_id, media_id) DO NOTHING
	`, subjectID, profilePicture); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE media SET file_name = '', updated_at = $2 WHERE uploaded_by = $1 AND file_name <> ''`, subjectID, at)
	return err
}

// RecordSharedProfilePicture records the person's current profile picture
// in account_deletion_media when club content also uses it, so
// profile_media_id stays empty: erase_profile_media then keeps it as club
// content by the rule (personalOnErasureSQL) and strips the person's file
// name from its object. It runs in anonymize_core's transaction; a rerun
// keeps the record already there.
func RecordSharedProfilePicture(ctx context.Context, tx pgx.Tx, subjectID, pictureID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO account_deletion_media (request_id, media_id)
		SELECT request.id, picture.id
		FROM account_deletion_requests request
		JOIN media picture ON picture.id = $2
		WHERE request.subject_id = $1 AND picture.blob_purged_at IS NULL
		ON CONFLICT (request_id, media_id) DO NOTHING
	`, subjectID, pictureID)
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

// EnsureErased erases a Media erase_profile_media names: an upload
// anonymize_core recorded, or the person's profile picture.
//
// A recorded upload is told apart by its purpose now (personalOnErasureSQL).
// The person's own is purged whatever still uses it, an Answer file a
// Skyforms response still holds included; its record goes in the
// transaction that purges it. Club content keeps its file: its object is
// served without the person's file name from then on, and its record goes.
// The profile picture is purged only once nothing uses it (the store's
// locked reference check), so a picture club content also uses keeps its
// object and loses only its uploader and name.
//
// The error goes to the erasure worker's log, so it names no Media, object,
// file or person (erasureError).
func (e *ImmediateBlobEraser) EnsureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	return erasureError(e.ensureErased(ctx, id, at))
}

func (e *ImmediateBlobEraser) ensureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	recorded, err := recordedForErasure(ctx, e.media.pool, id)
	if err != nil {
		return err
	}
	if recorded {
		return e.eraseUpload(ctx, id, at)
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

// eraseUpload ends the erasure of a recorded upload. The store's two-phase
// purge (erasedQueue), with its locks and durable claim, purges the
// person's own; club content is held back and kept (keepClub). A Media
// purged another way meanwhile only loses its record.
func (e *ImmediateBlobEraser) eraseUpload(ctx context.Context, id uuid.UUID, at time.Time) error {
	outcome, err := e.media.purge(ctx, id, at, erasedQueue, e.deleteObject(ctx))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordedMediaNotErased, err)
	}
	switch outcome {
	case purgeDone:
		return nil
	case purgeHeldBack:
		return e.keepClub(ctx, id)
	}
	item, err := e.media.GetIncludingDeleted(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err != nil || item.BlobPurgedAt != nil {
		return forgetErased(ctx, e.media.pool, id)
	}
	// Another purge holds its claim: try again later.
	return ErrRecordedMediaNotErased
}

// keepClub ends the erasure of club content: its public object is served
// without the person's file name from now on (only a download names one;
// the key never does), then its record goes. An object already gone needs
// no new metadata.
func (e *ImmediateBlobEraser) keepClub(ctx context.Context, id uuid.UUID) error {
	item, err := e.media.GetIncludingDeleted(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && item.Visibility == VisibilityPublic && item.BlobPurgedAt == nil {
		if meta := ServingMetadata(item.Type, ""); meta.ContentDisposition != "" {
			if err := e.blobs.SetMetadata(ctx, item.Key, meta); err != nil && !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: %w", ErrRecordedMediaNotErased, errObjectStorage{request: "metadata rewrite", cause: err})
			}
		}
	}
	return forgetErased(ctx, e.media.pool, id)
}

func (e *ImmediateBlobEraser) deleteObject(ctx context.Context) func(string) error {
	return func(key string) error {
		err := e.blobs.Delete(ctx, key)
		if err == nil || errors.Is(err, ErrPrivateMediaDisabled) {
			return err
		}
		return errObjectStorage{request: "delete", cause: err}
	}
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
