package media_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
)

// The cleanup worker removes an expired private Media from the private
// bucket, where it is, through the same object key.
func TestExpiredPrivateMediaIsPurgedFromThePrivateBucket(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	file := pm.answerFile(t, signedIn("92929292-9292-9292-9292-929292929292"))

	report, err := media.PurgeExpired(ctx, pm.store, media.Buckets{Public: pm.public, Private: pm.storage}, time.Now().Add(25*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pm.private.Get(file.Key); ok {
		t.Fatalf("the expired Answer file is still in the private bucket (report %+v)", report)
	}
}

func TestBucketsNeverHandAPrivateObjectToThePublicSide(t *testing.T) {
	t.Parallel()
	pm := newPrivateMedia(t)
	ctx := context.Background()
	file := pm.answerFile(t, signedIn("93939393-9393-9393-9393-939393939393"))
	buckets := media.Buckets{Public: pm.public, Private: pm.storage}

	if _, err := buckets.Read(ctx, file.Key); err == nil {
		t.Error("read a private object as a public one")
	}
	if err := buckets.Put(ctx, file.Key, []byte("plaintext"), media.BlobMetadata{}); err == nil {
		t.Error("wrote plaintext under a private key")
	}
	if err := buckets.SetMetadata(ctx, file.Key, media.BlobMetadata{ContentType: "application/pdf"}); err == nil {
		t.Error("gave a private object serving metadata")
	}
}

// With private Media off, a private object cannot be deleted: the purge
// fails and retries instead of recording a purge that did not happen.
func TestBucketsWithPrivateMediaOffDoNotDeletePrivateObjects(t *testing.T) {
	t.Parallel()
	err := media.Buckets{Public: media.NewMemoryBlob()}.Delete(context.Background(), "private/files/cv")
	if !errors.Is(err, media.ErrPrivateMediaDisabled) {
		t.Fatalf("err = %v, want %v", err, media.ErrPrivateMediaDisabled)
	}
}

// deleteRecorder is a bucket that records the keys it is asked to delete.
type deleteRecorder struct {
	*media.MemoryBlob
	deleted []string
}

func (d *deleteRecorder) Delete(ctx context.Context, key string) error {
	d.deleted = append(d.deleted, key)
	return d.MemoryBlob.Delete(ctx, key)
}

// What deletes by key alone (the purges, the staged upload sweeper, account
// erasure's erase_profile_media and erase_staged_uploads) reaches only the
// bucket that holds the object.
func TestBucketsDeleteFromTheBucketThatHoldsTheObject(t *testing.T) {
	t.Parallel()
	public := &deleteRecorder{MemoryBlob: media.NewMemoryBlob()}
	private := &deleteRecorder{MemoryBlob: media.NewMemoryBlob()}
	bao := transittest.NewServer(t)
	buckets := media.Buckets{Public: public, Private: media.NewPrivateStorage(private, transit.New(bao.Config()))}

	if err := buckets.Delete(context.Background(), "private/files/cv"); err != nil {
		t.Fatal(err)
	}
	if err := buckets.Delete(context.Background(), "images/cover"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(private.deleted, []string{"private/files/cv"}) || !slices.Equal(public.deleted, []string{"images/cover"}) {
		t.Fatalf("private deleted %v, public deleted %v", private.deleted, public.deleted)
	}
}

// Deleting what is not there is done, in either bucket, so an erasure step or
// a purge that runs again succeeds.
func TestBucketsDeleteOfAMissingObjectSucceeds(t *testing.T) {
	t.Parallel()
	public, publicS3 := fakeR2(t)
	private, privateS3 := fakeR2(t)
	publicS3.fail("/media/images/gone", "NoSuchKey")
	privateS3.fail("/media/private/files/gone", "NoSuchKey")
	bao := transittest.NewServer(t)
	buckets := media.Buckets{Public: public, Private: media.NewPrivateStorage(private, transit.New(bao.Config()))}

	for _, key := range []string{"images/gone", "private/files/gone", "images/never", "private/files/never"} {
		if err := buckets.Delete(context.Background(), key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}
