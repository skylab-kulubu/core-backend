package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Legacy Answer files (docs/media-lifecycle.md, "Legacy Answer files").
// Skyforms Answer files uploaded before Media purpose are legacy Media:
// public, unencrypted, never scanned, and held by no Media attachment, since
// Skyforms links only the Answer files it uploaded with a purpose. Skyforms
// keeps each answer's Media id in its own database; a read-only export of it
// (Media id, response id) tells core which legacy Media is whose Answer file.
// MoveLegacyAnswers moves each such Media into the private bucket,
// encrypted, as an answer_file waiting for its malware scan, and links it to
// its responses for Skyforms; once the scan has ended, a later run deletes
// its public object (queuing the CDN purge). Skyforms' code does not change:
// it already asks core for a read link when an Answer file is private.

// LegacyAnswerPair is one Skyforms response that holds a Media as one of its
// Answer files.
type LegacyAnswerPair struct {
	MediaID    uuid.UUID
	ResponseID uuid.UUID
}

// LegacyAnswerMaxBytes is the largest legacy Answer file the move takes: it
// holds the file and its ciphertext in memory, inside core's container.
// Media uploaded without a purpose were at most 20 MiB.
const LegacyAnswerMaxBytes = 100 << 20

// legacyAnswerOwnerType is the record type Skyforms gives a response's
// Media attachments (owner type "response", the response id as owner id).
const legacyAnswerOwnerType = "response"

// LegacyAnswerMode is what a run of MoveLegacyAnswers does.
type LegacyAnswerMode int

const (
	// LegacyAnswersCount only counts.
	LegacyAnswersCount LegacyAnswerMode = iota
	// LegacyAnswersMove moves Media into private storage (steps 1 and 2);
	// it deletes no public object.
	LegacyAnswersMove
	// LegacyAnswersFinalize deletes the public object of moved Media whose
	// scan has ended clean (attached) or infected (step 3).
	LegacyAnswersFinalize
)

// LegacyAnswerStorage is what the move reads and writes.
type LegacyAnswerStorage struct {
	// Public is the public bucket. Its deletes queue the CDN purge
	// (R2.PurgeCDNOnChange).
	Public interface {
		Size(ctx context.Context, key string) (int64, error)
		Read(ctx context.Context, key string) ([]byte, error)
		Delete(ctx context.Context, key string) error
	}
	// Private stores encrypted objects in the private bucket and reads them
	// back.
	Private interface {
		Seal(ctx context.Context, key string, plaintext []byte) (SealedObject, error)
		Read(ctx context.Context, obj SealedObject) ([]byte, error)
		Delete(ctx context.Context, key string) error
	}
}

// LegacyAnswerReport counts one run of MoveLegacyAnswers. It names no Media,
// person, file or address.
type LegacyAnswerReport struct {
	// Pairs and Media are what was read: responses' Answer files, and the
	// distinct Media among them.
	Pairs, Media int
	// ToMove Media are legacy Answer files the move takes (or took, with
	// LegacyAnswersMove, unless the limit left them for the next run),
	// ToMoveBytes their recorded size.
	ToMove      int
	ToMoveBytes int64
	// OutsideSizeRule and OutsideTypeRule of them break the answer_file
	// purpose's size or type rule; they are moved all the same (the rule is
	// for new uploads; the scan is what protects the reader).
	OutsideSizeRule, OutsideTypeRule int
	// Moved Media are done: moved, scanned, and their public object deleted.
	Moved int
	// Moved Media whose public object is still there: AwaitingScan wait for
	// their malware scan; ReadyToFinalize were found clean (attached) or
	// infected, and LegacyAnswersFinalize deletes their public object;
	// PublicKept were rejected for another reason (an archive the scan
	// cannot check, an integrity failure, a timeout…) or are clean but no
	// longer attached: their public object is kept, as the only copy of a
	// file that is not known to be malware.
	AwaitingScan, ReadyToFinalize, PublicKept int
	// Left alone: not a legacy public Media (uploaded with a purpose, or
	// moved some other way), not in core, archived or being purged, used by
	// core or another product too (or held by the purpose backfill),
	// a video, larger than LegacyAnswerMaxBytes, or whose public object is
	// gone.
	NotLegacy, NotFound, Archived, Shared, Video, TooLarge, ObjectMissing int
	// MovedNow Media this run moved; FinalizedNow public objects this run
	// deleted; Changed Media changed while they were moved (left as they
	// were); Remaining is the work the limit left for the next run.
	MovedNow, FinalizedNow, Changed, Remaining int
	// Failed Media are named through onError; the next run tries them again.
	Failed int
}

// legacyAnswerState is what the move finds a Media to be.
type legacyAnswerState int

const (
	answerNotFound legacyAnswerState = iota
	answerMoved
	answerAwaitingScan
	answerReadyToFinalize
	answerPublicKept
	answerNotLegacy
	answerArchived
	answerShared
	answerVideo
	answerTooLarge
	answerObjectMissing
	answerToMove
)

// legacyAnswer is one Media of the input: what the move found and the
// responses that hold it.
type legacyAnswer struct {
	media     Media
	state     legacyAnswerState
	publicKey string
	responses []uuid.UUID
}

// MoveLegacyAnswers works on the legacy Answer files that pairs name. With
// LegacyAnswersCount it only counts: it reads core's database and asks the
// public bucket whether each object still to move is there. The other modes
// work on at most limit Media, in Media id order, and count the rest as
// remaining: run them again until nothing remains.
//
// Each Media goes through three steps, each one safe to repeat:
//  1. (LegacyAnswersMove) its public object is read and stored encrypted in
//     the private bucket at private/<its key>, then read back and decrypted
//     to check that the copy holds the same bytes (size and SHA-256);
//  2. (LegacyAnswersMove) one transaction checks it is still a legacy public
//     Media that only Skyforms answers use, makes it a private answer_file
//     waiting for its scan (scanning, no expiry, the new key and its
//     encryption), records the move, and writes a Media attachment for each
//     response (forms, response, the response id, answer);
//  3. (LegacyAnswersFinalize) once the scan worker of the running core has
//     found it clean (and it is attached) or infected, its public object
//     (and any size of it) is deleted through the public bucket, which
//     queues the CDN purge, and the deletion is recorded. A Media the scan
//     rejected for any other reason keeps its public object: the scan has
//     deleted the private copy, and the file is not known to be malware.
//
// Its scan deadline runs from the move. A Media that fails is reported
// through onError and does not stop the others.
func MoveLegacyAnswers(ctx context.Context, store *PostgresStore, storage LegacyAnswerStorage, catalogue Catalogue, pairs []LegacyAnswerPair, mode LegacyAnswerMode, limit int, now func() time.Time, onError func(error)) (LegacyAnswerReport, error) {
	var report LegacyAnswerReport
	if mode != LegacyAnswersCount && limit < 1 {
		return report, fmt.Errorf("media: the limit must be at least 1")
	}
	if now == nil {
		now = time.Now
	}
	fail := func(id uuid.UUID, err error) {
		report.Failed++
		if onError != nil {
			onError(fmt.Errorf("media %s: %w", id, err))
		}
	}
	answerFile, ok := catalogue.Lookup(PurposeAnswerFile)
	if !ok {
		return report, fmt.Errorf("media: purpose %s is not in the catalogue", PurposeAnswerFile)
	}

	answers := groupLegacyAnswers(pairs)
	report.Pairs, report.Media = len(pairs), len(answers)
	var work []*legacyAnswer
	for _, answer := range answers {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		// Only a count asks the bucket: a move finds a missing object as it
		// reads it, and a finalize does not read it.
		if err := store.classifyLegacyAnswer(ctx, storage, answer, mode == LegacyAnswersCount); err != nil {
			fail(answer.media.ID, err)
			continue
		}
		switch answer.state {
		case answerNotFound:
			report.NotFound++
		case answerMoved:
			report.Moved++
		case answerAwaitingScan:
			report.AwaitingScan++
		case answerReadyToFinalize:
			report.ReadyToFinalize++
			if mode == LegacyAnswersFinalize {
				work = append(work, answer)
			}
		case answerPublicKept:
			report.PublicKept++
		case answerNotLegacy:
			report.NotLegacy++
		case answerArchived:
			report.Archived++
		case answerShared:
			report.Shared++
		case answerVideo:
			report.Video++
		case answerTooLarge:
			report.TooLarge++
		case answerObjectMissing:
			report.ObjectMissing++
		case answerToMove:
			report.ToMove++
			report.ToMoveBytes += answer.media.Size
			if answer.media.Size > answerFile.MaxBytes {
				report.OutsideSizeRule++
			}
			if !slices.Contains(answerFile.Types, answer.media.Type) {
				report.OutsideTypeRule++
			}
			if mode == LegacyAnswersMove {
				work = append(work, answer)
			}
		}
	}

	for i, answer := range work {
		if i >= limit {
			report.Remaining = len(work) - i
			break
		}
		if err := ctx.Err(); err != nil {
			report.Remaining = len(work) - i
			return report, err
		}
		if mode == LegacyAnswersFinalize {
			if err := deleteLegacyAnswerPublic(ctx, store, storage, answer.media.ID, answer.publicKey, now); err != nil {
				fail(answer.media.ID, fmt.Errorf("its public object is not deleted yet (the next run deletes it): %w", err))
				continue
			}
			report.FinalizedNow++
			continue
		}
		moved, err := moveLegacyAnswer(ctx, store, storage, answer, now)
		switch {
		case errors.Is(err, ErrNotFound):
			// The public object is gone.
			report.ObjectMissing++
		case err != nil:
			fail(answer.media.ID, err)
		case !moved:
			report.Changed++
		default:
			report.MovedNow++
		}
	}
	return report, nil
}

// groupLegacyAnswers gathers the responses of each Media, in Media id
// order, each response once.
func groupLegacyAnswers(pairs []LegacyAnswerPair) []*legacyAnswer {
	byID := map[uuid.UUID]*legacyAnswer{}
	for _, pair := range pairs {
		answer, ok := byID[pair.MediaID]
		if !ok {
			answer = &legacyAnswer{media: Media{ID: pair.MediaID}}
			byID[pair.MediaID] = answer
		}
		if !slices.Contains(answer.responses, pair.ResponseID) {
			answer.responses = append(answer.responses, pair.ResponseID)
		}
	}
	answers := make([]*legacyAnswer, 0, len(byID))
	for _, answer := range byID {
		answers = append(answers, answer)
	}
	slices.SortFunc(answers, func(a, b *legacyAnswer) int { return bytes.Compare(a.media.ID[:], b.media.ID[:]) })
	return answers
}

// classifyLegacyAnswer reads the Media and sets what the move finds it to
// be. With head, a Media to move has its public object looked for.
func (s *PostgresStore) classifyLegacyAnswer(ctx context.Context, storage LegacyAnswerStorage, answer *legacyAnswer, head bool) error {
	m, err := s.GetIncludingDeleted(ctx, answer.media.ID)
	if errors.Is(err, ErrNotFound) {
		answer.state = answerNotFound
		return nil
	}
	if err != nil {
		return err
	}
	answer.media = m
	move, err := s.legacyAnswerMove(ctx, m.ID)
	if err != nil {
		return err
	}
	if move != nil {
		answer.publicKey = move.publicKey
		answer.state = movedState(m, move.publicDeleted)
		return nil
	}
	answer.publicKey = m.Key
	switch {
	case m.Purpose != PurposeLegacy || m.Visibility != VisibilityPublic:
		answer.state = answerNotLegacy
		return nil
	case m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil:
		answer.state = answerArchived
		return nil
	}
	shared, err := s.legacyAnswerShared(ctx, m.ID)
	if err != nil {
		return err
	}
	_, video := videoOriginalOf(m.Key)
	switch {
	case shared || m.DetachExpiryHeld:
		answer.state = answerShared
	case video:
		// A video's faststart copies live beside it; an Answer file never
		// was one.
		answer.state = answerVideo
	case m.Size > LegacyAnswerMaxBytes:
		answer.state = answerTooLarge
	case !head:
		answer.state = answerToMove
	default:
		if _, err := storage.Public.Size(ctx, m.Key); errors.Is(err, ErrNotFound) {
			answer.state = answerObjectMissing
		} else if err != nil {
			return fmt.Errorf("public object: %w", err)
		} else {
			answer.state = answerToMove
		}
	}
	return nil
}

// movedState is where a moved Media is: done once its public object is
// deleted; otherwise waiting for its scan, ready for its public object to go
// (clean and attached, or infected), or keeping it (rejected for another
// reason, or clean but no longer attached).
func movedState(m Media, publicDeleted bool) legacyAnswerState {
	switch {
	case publicDeleted:
		return answerMoved
	case m.Status == StatusScanning:
		return answerAwaitingScan
	case m.Status == StatusAttached && m.ScanResult == ScanClean:
		return answerReadyToFinalize
	case m.Status == StatusRejected && m.ScanResult == ScanInfected:
		return answerReadyToFinalize
	default:
		return answerPublicKept
	}
}

// moveLegacyAnswer makes steps 1 and 2 of the move. moved is false, and the
// Media is left as it was, when it changed since it was read (attached by
// core, archived, moved by another run); the private copy is then deleted
// unless the Media points at it. ErrNotFound: the public object is gone.
func moveLegacyAnswer(ctx context.Context, store *PostgresStore, storage LegacyAnswerStorage, answer *legacyAnswer, now func() time.Time) (bool, error) {
	m := answer.media
	data, err := storage.Public.Read(ctx, m.Key)
	if err != nil {
		return false, err
	}
	if int64(len(data)) != m.Size {
		return false, fmt.Errorf("its public object is %d bytes, its record says %d: left alone", len(data), m.Size)
	}
	sum := sha256.Sum256(data)
	privateKey := PrivateObjectKey(m.Key)
	sealed, err := storage.Private.Seal(ctx, privateKey, data)
	clear(data)
	if err != nil {
		return false, err
	}
	// The copy is read back before the Media points at it: the public object
	// is deleted later on its strength.
	copied, err := storage.Private.Read(ctx, sealed)
	if err == nil && (int64(len(copied)) != m.Size || sha256.Sum256(copied) != sum) {
		err = errors.New("the private copy does not hold the public object's bytes")
	}
	clear(copied)
	if err != nil {
		if deleteErr := storage.Private.Delete(ctx, privateKey); deleteErr != nil {
			err = errors.Join(err, deleteErr)
		}
		return false, fmt.Errorf("private copy not verified, left alone: %w", err)
	}
	moved, err := store.moveLegacyAnswer(ctx, m, sealed, answer.responses, now().UTC())
	if err != nil || moved {
		// An error may hide a commit that went through: the copy stays, and
		// the next run sees what the database says.
		return moved, err
	}
	current, err := store.GetIncludingDeleted(ctx, m.ID)
	if err == nil && current.Key == privateKey {
		return false, nil
	}
	if err := storage.Private.Delete(ctx, privateKey); err != nil {
		return false, fmt.Errorf("changed while it was moved; its private copy is not deleted: %w", err)
	}
	return false, nil
}

// deleteLegacyAnswerPublic makes step 3: deletes the public object of a
// moved Media, and any size of it, through the public bucket (each delete
// queues the CDN purge; deleting what is gone succeeds), then records it.
func deleteLegacyAnswerPublic(ctx context.Context, store *PostgresStore, storage LegacyAnswerStorage, id uuid.UUID, publicKey string, now func() time.Time) error {
	if err := purgeObjects(publicKey, func(key string) error { return storage.Public.Delete(ctx, key) }); err != nil {
		return err
	}
	return store.recordLegacyAnswerPublicDeleted(ctx, id, now().UTC())
}

// ReadLegacyAnswerPairs reads the export of Skyforms' Answer files: one
// "<Media id>\t<response id>" per line. Empty lines, '#' lines and a header
// whose first column is media_id are skipped. A line it cannot read is an
// error naming the line, never its content.
func ReadLegacyAnswerPairs(in io.Reader) ([]LegacyAnswerPair, error) {
	var pairs []LegacyAnswerPair
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimRight(scanner.Text(), "\r")
		first, rest, tabbed := strings.Cut(text, "\t")
		first = strings.TrimSpace(first)
		if first == "" || first == "media_id" || strings.HasPrefix(first, "#") {
			continue
		}
		second := strings.TrimSpace(rest)
		if !tabbed || second == "" || strings.Contains(second, "\t") {
			return nil, fmt.Errorf("line %d: want two tab-separated columns, the Media id and the response id", line)
		}
		mediaID, err := uuid.Parse(first)
		if err != nil || mediaID == uuid.Nil {
			return nil, fmt.Errorf("line %d: the first column is not a Media id", line)
		}
		responseID, err := uuid.Parse(second)
		if err != nil || responseID == uuid.Nil {
			return nil, fmt.Errorf("line %d: the second column is not a response id", line)
		}
		pairs = append(pairs, LegacyAnswerPair{MediaID: mediaID, ResponseID: responseID})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("standard input: %w", err)
	}
	return pairs, nil
}
