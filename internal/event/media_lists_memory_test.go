package event_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func itemIDs(items []event.MediaItem) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// The memory store answers an Event's files and videos as the database
// does: from the Media it reads them from, an archived one left out, an
// address only for one that can be served, and counts of those alone. So a
// list's count and what anyone but the Event's editors sees always agree.
func TestMemoryStoreListsAndCountsTheFilesItCanServe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mediaStore := media.NewMemoryStore()
	stored := func(status media.Status, visibility media.Visibility) media.Media {
		t.Helper()
		m, err := mediaStore.Create(ctx, media.Media{
			Name: "a.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/" + uuid.NewString(), Size: 7,
			UploadedBy: uuid.New(), Purpose: media.PurposeClubFile, Status: status, Visibility: visibility,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	servable := stored(media.StatusAttached, media.VisibilityPublic)
	scanning := stored(media.StatusScanning, media.VisibilityPublic)
	private := stored(media.StatusAttached, media.VisibilityPrivate)
	archived := stored(media.StatusAttached, media.VisibilityPublic)
	if err := mediaStore.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	video := stored(media.StatusAttached, media.VisibilityPublic)

	store := event.NewMemoryStore().ReadMediaFrom(mediaStore)
	created, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFiles(ctx, created.ID, event.Files, []uuid.UUID{servable.ID, scanning.ID, private.ID, archived.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFiles(ctx, created.ID, event.Videos, []uuid.UUID{video.ID}); err != nil {
		t.Fatal(err)
	}

	files, videos, err := store.ListFiles(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemIDs(files); !slices.Equal(got, []uuid.UUID{servable.ID, scanning.ID, private.ID}) {
		t.Fatalf("files %v, want the servable, the scanning and the private one (the archived one left out)", got)
	}
	if files[0].URL != servable.Key || files[0].Name != "a.pdf" || files[0].Size != 7 || files[0].Status != media.StatusAttached ||
		files[1].URL != "" || files[1].Status != media.StatusScanning || files[2].URL != "" {
		t.Fatalf("files %+v", files)
	}
	if len(videos) != 1 || videos[0].URL != video.Key {
		t.Fatalf("videos %+v", videos)
	}
	listed, err := store.List(ctx, "", false)
	if err != nil || len(listed) != 1 || listed[0].Files != nil || listed[0].FileCount != 1 || listed[0].VideoCount != 1 {
		t.Fatalf("list %+v (err %v), want counts 1 and 1 and no lists", listed, err)
	}

	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	detail, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	public := svc.ProjectFor(nil, detail)
	if len(public.Files) != public.FileCount || len(public.Videos) != public.VideoCount || public.FileCount != 1 {
		t.Fatalf("anyone sees %d files and %d videos, counted %d and %d", len(public.Files), len(public.Videos), public.FileCount, public.VideoCount)
	}

	// Ordering names the list as it is shown; removing takes all or none.
	if _, err := store.OrderFiles(ctx, created.ID, event.Files, []uuid.UUID{private.ID, servable.ID}); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("an order missing an item: %v, want a conflict", err)
	}
	if _, err := store.OrderFiles(ctx, created.ID, event.Files, []uuid.UUID{private.ID, private.ID, servable.ID}); !errors.Is(err, event.ErrInvalid) {
		t.Fatalf("an order naming an item twice: %v", err)
	}
	ordered, err := store.OrderFiles(ctx, created.ID, event.Files, []uuid.UUID{private.ID, scanning.ID, servable.ID})
	if err != nil || !slices.Equal(itemIDs(ordered.Files), []uuid.UUID{private.ID, scanning.ID, servable.ID}) {
		t.Fatalf("ordered %v (err %v)", itemIDs(ordered.Files), err)
	}
	if _, err := store.RemoveFiles(ctx, created.ID, event.Files, []uuid.UUID{scanning.ID, video.ID}); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("removing one not in the list: %v", err)
	}
	removed, err := store.RemoveFiles(ctx, created.ID, event.Files, []uuid.UUID{scanning.ID})
	if err != nil || !slices.Equal(itemIDs(removed.Files), []uuid.UUID{private.ID, servable.ID}) || removed.FileCount != 1 {
		t.Fatalf("after removing: %v, count %d (err %v)", itemIDs(removed.Files), removed.FileCount, err)
	}

	// Without the Media to read, an item is its id alone: nobody but the
	// Event's editors sees it, and nothing counts it.
	bare := event.NewMemoryStore()
	other, _ := bare.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	added, err := bare.AddFiles(ctx, other.ID, event.Files, []uuid.UUID{servable.ID})
	if err != nil || len(added.Files) != 1 || added.Files[0].URL != "" || added.FileCount != 0 {
		t.Fatalf("a store without Media answers %+v, count %d (err %v)", added.Files, added.FileCount, err)
	}
}

// The memory store answers a video's poster as the database does: from the
// Media it reads it from, none for an archived one, an address only for
// one that can be served. The poster stays through a reorder, clearing
// takes it, a video the Event does not list has none to set, and the Team
// media library sees it.
func TestMemoryStoreAnswersAVideosPoster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mediaStore := media.NewMemoryStore()
	stored := func(purpose, contentType, key string) media.Media {
		t.Helper()
		m, err := mediaStore.Create(ctx, media.Media{
			Name: "x", Type: contentType, Kind: media.KindImage, Key: key, Size: 7, Width: 800, Height: 600,
			UploadedBy: uuid.New(), Purpose: purpose, Status: media.StatusAttached, Visibility: media.VisibilityPublic,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := stored(media.PurposeVideo, "video/mp4", "videos/a.mp4")
	second := stored(media.PurposeVideo, "video/mp4", "videos/b.mp4")
	poster := stored(media.PurposeEventCover, "image/png", "images/kapak")
	archived := stored(media.PurposeEventGallery, "image/png", "images/arsiv")
	if err := mediaStore.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}

	store := event.NewMemoryStore().ReadMediaFrom(mediaStore)
	created, err := store.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddFiles(ctx, created.ID, event.Videos, []uuid.UUID{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetVideoPoster(ctx, created.ID, first.ID, &poster.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetVideoPoster(ctx, created.ID, second.ID, &archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetVideoPoster(ctx, created.ID, poster.ID, &poster.ID); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("a poster for a video the Event does not list: %v", err)
	}
	if _, err := store.OrderFiles(ctx, created.ID, event.Videos, []uuid.UUID{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}

	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	detail, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := media.ImageAddress{URL: "https://cdn.example.test/images/kapak", Width: 800, Height: 600}
	want := &event.Poster{ID: poster.ID, URL: original.URL, Sizes: map[string]media.ImageAddress{media.SizeCard: original, media.SizePage: original}}
	if len(detail.Videos) != 2 || detail.Videos[0].Poster != nil || !reflect.DeepEqual(detail.Videos[1].Poster, want) {
		t.Fatalf("videos %+v, want the second without a poster (archived) and the first with %+v", detail.Videos, want)
	}
	if teams, err := store.TeamsUsingMedia(ctx, poster.ID, uuid.New()); err != nil || !slices.Equal(teams, []string{"WEBLAB"}) {
		t.Fatalf("teams using the poster %v (err %v)", teams, err)
	}

	cleared, err := store.SetVideoPoster(ctx, created.ID, first.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if published, _ := svc.Get(ctx, cleared.ID); published.Videos[1].Poster != nil {
		t.Fatalf("a cleared poster is still answered: %+v", published.Videos[1].Poster)
	}
}
