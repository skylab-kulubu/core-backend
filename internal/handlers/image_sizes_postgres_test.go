package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const sizesBase = "https://cdn.example.test"

// imageSizesFixture reads Events and profiles from Postgres, where a record
// reads the Media it links in its own query, and counts the queries.
type imageSizesFixture struct {
	pool     *pgxpool.Pool
	queries  *queryCounter
	media    *media.PostgresStore
	events   *event.PostgresStore
	users    *user.PostgresStore
	uploader uuid.UUID
	app      *fiber.App
}

func newImageSizesFixture(t *testing.T) imageSizesFixture {
	t.Helper()
	return newImageSizesFixtureInMode(t, "")
}

// newImageSizesFixtureInMode serves Events whose sizes point where mode
// says (MEDIA_IMAGE_ADDRESS_MODE).
func newImageSizesFixtureInMode(t *testing.T, mode media.AddressMode) imageSizesFixture {
	t.Helper()
	ctx := context.Background()
	plain := testpostgres.Start(t)
	if err := migrate.Apply(ctx, plain); err != nil {
		t.Fatal(err)
	}
	config := plain.Config()
	queries := &queryCounter{}
	config.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	users := user.NewPostgresStore(pool)
	uploader := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, uploader, user.Profile{Email: "organizer@example.test", FirstName: "Org", LastName: "Anizer"}); err != nil {
		t.Fatal(err)
	}
	mediaStore := media.NewPostgresStore(pool)
	events := event.NewPostgresStore(pool)
	svc := event.NewServiceWithOptions(events, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{
		PublicBase:       sizesBase,
		ImageAddressMode: mode,
		Media:            media.NewLinker(mediaStore),
	})
	return imageSizesFixture{
		pool: pool, queries: queries, media: mediaStore, events: events, users: users, uploader: uploader,
		app: eventServiceApp(t, authn.Identity{}, svc),
	}
}

// image stores an image Media as core records it once its sizes are made:
// sizes names the size objects stored beside it (nil: none recorded).
func (f imageSizesFixture) image(t *testing.T, purpose, key string, width, height int, sizes map[string]media.SizeObject) media.Media {
	t.Helper()
	m, err := f.media.Create(context.Background(), media.Media{
		Name: "photo.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: key, UploadedBy: f.uploader,
		Purpose: purpose, Width: width, Height: height, SizeObjects: sizes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// download stores a Media of an Event's files or videos as a completed
// Direct upload records it.
func (f imageSizesFixture) download(t *testing.T, purpose string) media.Media {
	t.Helper()
	contentType, key := "application/pdf", "files/"+uuid.NewString()
	if purpose == media.PurposeVideo {
		contentType, key = "video/mp4", "videos/"+uuid.NewString()+".mp4"
	}
	m, err := f.media.Create(context.Background(), media.Media{
		Name: "x", Type: contentType, Kind: media.KindFile, Key: key, Size: 10, UploadedBy: f.uploader, Purpose: purpose,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f imageSizesFixture) event(t *testing.T, name string, cover *uuid.UUID, gallery ...uuid.UUID) event.Event {
	t.Helper()
	ctx := context.Background()
	created, err := f.events.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true, CoverImageID: cover})
	if err != nil {
		t.Fatal(err)
	}
	if len(gallery) > 0 {
		if created, err = f.events.AddImages(ctx, created.ID, gallery); err != nil {
			t.Fatal(err)
		}
	}
	return created
}

func (f imageSizesFixture) get(t *testing.T, path string, into any) {
	t.Helper()
	answer(t, f.app, httptest.NewRequest(fiber.MethodGet, path, nil), into)
}

// answer sends req and reads its 200 JSON answer into into.
func answer(t *testing.T, app *fiber.App, req *http.Request, into any) {
	t.Helper()
	path := req.URL.Path
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("%s %s: %d %s", req.Method, path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s %s: %s: %v", req.Method, path, raw, err)
	}
}

type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// sizedEventView is an Event answer's images with their sizes.
type sizedEventView struct {
	ID              uuid.UUID                     `json:"id"`
	CoverImageURL   string                        `json:"coverImageUrl"`
	CoverImageSizes map[string]media.ImageAddress `json:"coverImageSizes"`
	Images          []struct {
		ID    uuid.UUID                     `json:"id"`
		URL   string                        `json:"url"`
		Sizes map[string]media.ImageAddress `json:"sizes"`
	} `json:"images"`
	ImageURLs []string `json:"imageUrls"`
	// Files and Videos are only in an Event's detail; a list carries the
	// counts.
	Files      []json.RawMessage `json:"files"`
	Videos     []json.RawMessage `json:"videos"`
	FileCount  int               `json:"fileCount"`
	VideoCount int               `json:"videoCount"`
}

func bothSizes(card, page media.ImageAddress) map[string]media.ImageAddress {
	return map[string]media.ImageAddress{media.SizeCard: card, media.SizePage: page}
}

func jpegSize(width, height int) media.SizeObject {
	return media.SizeObject{ImageSize: media.ImageSize{Width: width, Height: height}, Type: "image/jpeg"}
}

// An Event's cover carries its card and page addresses next to the full-size
// address, built like the Media's own sizes.
func TestEventDetailCarriesTheCoverSizesHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	cover := f.image(t, media.PurposeEventCover, "images/cover", 1600, 1200, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
	})
	created := f.event(t, "Hack", &cover.ID)

	var got sizedEventView
	f.get(t, "/v1/events/"+created.ID.String(), &got)

	if got.CoverImageURL != sizesBase+"/images/cover" {
		t.Fatalf("coverImageUrl %q", got.CoverImageURL)
	}
	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/images/cover/card.jpg", Width: 400, Height: 300},
		media.ImageAddress{URL: sizesBase + "/images/cover/page.jpg", Width: 1200, Height: 900},
	)
	if !reflect.DeepEqual(got.CoverImageSizes, want) {
		t.Fatalf("coverImageSizes %+v, want %+v", got.CoverImageSizes, want)
	}
}

// A cover without sizes of its own (a Media uploaded without a purpose)
// answers the original at every size, so a client never builds one.
func TestEventCoverWithoutSizesFallsBackToTheOriginalHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	legacy := f.image(t, media.PurposeLegacy, "images/legacy-cover", 0, 0, nil)
	created := f.event(t, "Old", &legacy.ID)

	var got sizedEventView
	f.get(t, "/v1/events/"+created.ID.String(), &got)

	original := media.ImageAddress{URL: sizesBase + "/images/legacy-cover"}
	if got.CoverImageURL != original.URL {
		t.Fatalf("coverImageUrl %q", got.CoverImageURL)
	}
	if want := bothSizes(original, original); !reflect.DeepEqual(got.CoverImageSizes, want) {
		t.Fatalf("coverImageSizes %+v, want %+v", got.CoverImageSizes, want)
	}
}

// An image smaller than the page size gets no page object: its page address
// is the original, at its own size.
func TestEventCoverSmallerThanThePageSizeAnswersTheOriginalAsPageHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	small := f.image(t, media.PurposeEventCover, "images/small-cover", 800, 600, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 300),
	})
	created := f.event(t, "Small", &small.ID)

	var got sizedEventView
	f.get(t, "/v1/events/"+created.ID.String(), &got)

	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/images/small-cover/card.jpg", Width: 400, Height: 300},
		media.ImageAddress{URL: sizesBase + "/images/small-cover", Width: 800, Height: 600},
	)
	if !reflect.DeepEqual(got.CoverImageSizes, want) {
		t.Fatalf("coverImageSizes %+v, want %+v", got.CoverImageSizes, want)
	}
}

// Every gallery image carries its sizes beside its address, in the shape of
// the Media JSON's sizes; imageUrls stays the full-size addresses.
func TestEventGalleryImagesCarryTheirSizesHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	sized := f.image(t, media.PurposeEventGallery, "images/gallery-sized", 1600, 1200, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
	})
	legacy := f.image(t, media.PurposeLegacy, "images/gallery-legacy", 0, 0, nil)
	created := f.event(t, "Gallery", nil, sized.ID, legacy.ID)

	var got sizedEventView
	f.get(t, "/v1/events/"+created.ID.String(), &got)

	if got.CoverImageSizes != nil {
		t.Fatalf("an Event without a cover has cover sizes %+v", got.CoverImageSizes)
	}
	legacyOriginal := media.ImageAddress{URL: sizesBase + "/images/gallery-legacy"}
	want := map[uuid.UUID]struct {
		url   string
		sizes map[string]media.ImageAddress
	}{
		sized.ID: {sizesBase + "/images/gallery-sized", bothSizes(
			media.ImageAddress{URL: sizesBase + "/images/gallery-sized/card.jpg", Width: 400, Height: 300},
			media.ImageAddress{URL: sizesBase + "/images/gallery-sized/page.jpg", Width: 1200, Height: 900},
		)},
		legacy.ID: {legacyOriginal.URL, bothSizes(legacyOriginal, legacyOriginal)},
	}
	if len(got.Images) != 2 {
		t.Fatalf("images %+v", got.Images)
	}
	for _, image := range got.Images {
		if image.URL != want[image.ID].url {
			t.Errorf("image %s url %q, want %q", image.ID, image.URL, want[image.ID].url)
		}
		if !reflect.DeepEqual(image.Sizes, want[image.ID].sizes) {
			t.Errorf("image %s sizes %+v, want %+v", image.ID, image.Sizes, want[image.ID].sizes)
		}
	}
	if len(got.ImageURLs) != 2 || got.ImageURLs[0] != got.Images[0].URL || got.ImageURLs[1] != got.Images[1].URL {
		t.Fatalf("imageUrls %v, images %+v", got.ImageURLs, got.Images)
	}
}

// The Event list answers each Event's cover and gallery sizes, and the
// counts of its files and videos, in three queries, for four Events as for
// one: the Events with their covers and counts, every listed Event's gallery
// with its Media, and every listed Event's door staff. Never one query per
// Event or per Media, and none for the videos' posters, which a list does
// not carry.
func TestEventListCarriesEveryEventsSizesWithoutAQueryPerEventHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	type seeded struct {
		cover, gallery media.Media
	}
	seed := func(i int) (event.Event, seeded) {
		key := "images/list-" + string(rune('a'+i))
		cover := f.image(t, media.PurposeEventCover, key+"-cover", 1600, 1200, map[string]media.SizeObject{
			media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
		})
		gallery := f.image(t, media.PurposeEventGallery, key+"-gallery", 1000, 750, map[string]media.SizeObject{
			media.SizeCard: jpegSize(400, 300),
		})
		legacy := f.image(t, media.PurposeLegacy, key+"-legacy", 0, 0, nil)
		created := f.event(t, "Event "+key, &cover.ID, gallery.ID, legacy.ID)
		// Two files and a video with a poster, so the list reads their
		// counts too.
		for list, purposes := range map[event.MediaList][]string{
			event.Files:  {media.PurposeClubFile, media.PurposeClubFile},
			event.Videos: {media.PurposeVideo},
		} {
			ids := make([]uuid.UUID, 0, len(purposes))
			for _, purpose := range purposes {
				ids = append(ids, f.download(t, purpose).ID)
			}
			if _, err := f.events.AddFiles(context.Background(), created.ID, list, ids); err != nil {
				t.Fatal(err)
			}
			if list == event.Videos {
				if _, err := f.events.SetVideoPoster(context.Background(), created.ID, ids[0], &gallery.ID, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		return created, seeded{cover: cover, gallery: gallery}
	}
	list := func() ([]sizedEventView, int64) {
		t.Helper()
		var got []sizedEventView
		f.queries.n.Store(0)
		f.get(t, "/v1/events", &got)
		return got, f.queries.n.Load()
	}

	first, _ := seed(0)
	one, oneQueries := list()
	if len(one) != 1 || one[0].ID != first.ID {
		t.Fatalf("list %+v", one)
	}
	if oneQueries != 3 {
		t.Fatalf("listing 1 Event took %d queries, want 3", oneQueries)
	}

	seededByEvent := map[uuid.UUID]seeded{}
	for i := 1; i < 4; i++ {
		created, images := seed(i)
		seededByEvent[created.ID] = images
	}
	four, fourQueries := list()
	if len(four) != 4 {
		t.Fatalf("list of %d Events", len(four))
	}
	if fourQueries != oneQueries {
		t.Fatalf("listing 4 Events took %d queries, 1 Event took %d", fourQueries, oneQueries)
	}

	for _, got := range append(four, one...) {
		if got.Files != nil || got.Videos != nil || got.FileCount != 2 || got.VideoCount != 1 {
			t.Errorf("Event %s listed with files %v, videos %v, counts %d and %d; want the counts alone, 2 and 1",
				got.ID, got.Files, got.Videos, got.FileCount, got.VideoCount)
		}
	}
	for _, got := range four {
		want, ok := seededByEvent[got.ID]
		if !ok {
			continue
		}
		coverKey := sizesBase + "/" + want.cover.Key
		if !reflect.DeepEqual(got.CoverImageSizes, bothSizes(
			media.ImageAddress{URL: coverKey + "/card.jpg", Width: 400, Height: 300},
			media.ImageAddress{URL: coverKey + "/page.jpg", Width: 1200, Height: 900},
		)) {
			t.Errorf("Event %s cover sizes %+v", got.ID, got.CoverImageSizes)
		}
		if len(got.Images) != 2 {
			t.Fatalf("Event %s images %+v", got.ID, got.Images)
		}
		for _, image := range got.Images {
			if image.ID != want.gallery.ID {
				if image.Sizes[media.SizeCard].URL != image.URL || image.Sizes[media.SizePage].URL != image.URL {
					t.Errorf("Event %s legacy image sizes %+v", got.ID, image.Sizes)
				}
				continue
			}
			galleryKey := sizesBase + "/" + want.gallery.Key
			if !reflect.DeepEqual(image.Sizes, bothSizes(
				media.ImageAddress{URL: galleryKey + "/card.jpg", Width: 400, Height: 300},
				media.ImageAddress{URL: galleryKey, Width: 1000, Height: 750},
			)) {
				t.Errorf("Event %s gallery sizes %+v", got.ID, image.Sizes)
			}
		}
	}
}

// sizedProfileView is a profile answer's picture with its sizes.
type sizedProfileView struct {
	ProfilePictureID    *uuid.UUID                    `json:"profilePictureId"`
	ProfilePictureURL   string                        `json:"profilePictureUrl"`
	ProfilePictureSizes map[string]media.ImageAddress `json:"profilePictureSizes"`
}

// profile is a person whose profile links picture (none when nil), and
// their /v1/users/me routes.
func (f imageSizesFixture) profile(t *testing.T, picture *media.Media) (*fiber.App, media.Service) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	profile := user.Profile{Email: id.String() + "@example.test", FirstName: "Ada", LastName: "Lovelace"}
	users := user.NewService(f.users)
	if _, _, err := users.Ensure(ctx, id, profile); err != nil {
		t.Fatal(err)
	}
	if picture != nil {
		if _, err := users.SetProfilePicture(ctx, id, picture.ID, picture.Key); err != nil {
			t.Fatal(err)
		}
	}
	svc := media.NewService(f.media, media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), sizesBase)
	return meIdentApp(t, f.users, id, profile, svc), svc
}

// A person's own profile answers their picture's card and page addresses
// next to its full-size address.
func TestMeCarriesTheProfilePictureSizesHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	picture := f.image(t, media.PurposeProfilePicture, "images/portrait", 1600, 1600, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 400), media.SizePage: jpegSize(1200, 1200),
	})
	app, _ := f.profile(t, &picture)

	var got sizedProfileView
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil), &got)

	if got.ProfilePictureURL != sizesBase+"/images/portrait" {
		t.Fatalf("profilePictureUrl %q", got.ProfilePictureURL)
	}
	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/images/portrait/card.jpg", Width: 400, Height: 400},
		media.ImageAddress{URL: sizesBase + "/images/portrait/page.jpg", Width: 1200, Height: 1200},
	)
	if !reflect.DeepEqual(got.ProfilePictureSizes, want) {
		t.Fatalf("profilePictureSizes %+v, want %+v", got.ProfilePictureSizes, want)
	}
}

// A picture stored without sizes of its own answers the original at every
// size; a profile without a picture answers none.
func TestMeProfilePictureWithoutSizesFallsBackToTheOriginalHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	legacy := f.image(t, media.PurposeLegacy, "images/legacy-portrait", 0, 0, nil)
	app, _ := f.profile(t, &legacy)

	var got sizedProfileView
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil), &got)

	original := media.ImageAddress{URL: sizesBase + "/images/legacy-portrait"}
	if got.ProfilePictureURL != original.URL {
		t.Fatalf("profilePictureUrl %q", got.ProfilePictureURL)
	}
	if want := bothSizes(original, original); !reflect.DeepEqual(got.ProfilePictureSizes, want) {
		t.Fatalf("profilePictureSizes %+v, want %+v", got.ProfilePictureSizes, want)
	}

	bare, _ := f.profile(t, nil)
	var none sizedProfileView
	answer(t, bare, httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil), &none)
	if none.ProfilePictureURL != "" || none.ProfilePictureSizes != nil {
		t.Fatalf("a profile without a picture answers %+v", none)
	}
}

// Uploading a picture answers its sizes at once: the ones the Media JSON
// gives the new Media.
func TestMeProfilePictureUploadAnswersTheNewPicturesSizesHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	app, svc := f.profile(t, nil)
	body, ctype := multipartPNG(t, "image", "portrait.png", grayPNGHTTP(t, 1600, 1200))
	req := httptest.NewRequest(fiber.MethodPost, "/v1/users/me/profile-picture", body)
	req.Header.Set("Content-Type", ctype)

	var got sizedProfileView
	answer(t, app, req, &got)

	if got.ProfilePictureID == nil {
		t.Fatalf("upload answered %+v", got)
	}
	uploaded, err := svc.Get(context.Background(), authz.Principal{}, *got.ProfilePictureID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfilePictureURL != uploaded.URL {
		t.Fatalf("profilePictureUrl %q, the Media's %q", got.ProfilePictureURL, uploaded.URL)
	}
	if len(uploaded.Sizes) != 2 || !reflect.DeepEqual(got.ProfilePictureSizes, uploaded.Sizes) {
		t.Fatalf("profilePictureSizes %+v, the Media's sizes %+v", got.ProfilePictureSizes, uploaded.Sizes)
	}
	if card := got.ProfilePictureSizes[media.SizeCard]; card.URL != uploaded.URL+"/card.png" || card.Width != 400 || card.Height != 300 {
		t.Fatalf("card %+v", card)
	}
}

// A public team roster answers each member's picture sizes, from the base
// core is configured with, like the picture's address.
func TestTeamRosterCarriesEachMembersPictureSizesHTTP(t *testing.T) {
	restore := media.UsePublicBase(sizesBase)
	t.Cleanup(restore)
	f := newImageSizesFixture(t)
	ctx := context.Background()
	dir := identity.NewMemory()
	seedPublicTeam(t, dir)
	members, err := dir.Members(ctx, "g-weblab")
	if err != nil || len(members) != 1 {
		t.Fatalf("members %+v %v", members, err)
	}
	ada := members[0]
	picture := f.image(t, media.PurposeProfilePicture, "images/ada", 1600, 1600, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 400), media.SizePage: jpegSize(1200, 1200),
	})
	users := user.NewService(f.users)
	if _, _, err := users.Ensure(ctx, ada.ID, user.Profile{Email: ada.Email, FirstName: ada.FirstName, LastName: ada.LastName}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.SetProfilePicture(ctx, ada.ID, picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	h := NewTeamHandler(identity.NewService(dir, f.users, authz.NewAuthorizer(authz.DefaultPolicy())))
	app := fiber.New()
	app.Get("/v1/teams/:team/members", h.Members)

	var got struct {
		Members []struct {
			FirstName           string                        `json:"firstName"`
			ProfilePictureURL   string                        `json:"profilePictureUrl"`
			ProfilePictureSizes map[string]media.ImageAddress `json:"profilePictureSizes"`
		} `json:"members"`
	}
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/teams/WEBLAB/members", nil), &got)

	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/images/ada/card.jpg", Width: 400, Height: 400},
		media.ImageAddress{URL: sizesBase + "/images/ada/page.jpg", Width: 1200, Height: 1200},
	)
	var found bool
	for _, member := range got.Members {
		if member.FirstName != ada.FirstName {
			if member.ProfilePictureSizes != nil {
				t.Errorf("%s has no picture but sizes %+v", member.FirstName, member.ProfilePictureSizes)
			}
			continue
		}
		found = true
		if member.ProfilePictureURL != sizesBase+"/images/ada" || !reflect.DeepEqual(member.ProfilePictureSizes, want) {
			t.Fatalf("%s: %q %+v, want sizes %+v", member.FirstName, member.ProfilePictureURL, member.ProfilePictureSizes, want)
		}
	}
	if !found {
		t.Fatalf("roster %+v", got.Members)
	}
}

// With Cloudflare image transformations configured, an Event's sizes point
// where the Media JSON's do: the Event service gets the address mode core
// starts with, like its base.
func TestEventCoverSizesFollowTheConfiguredAddressModeHTTP(t *testing.T) {
	f := newImageSizesFixtureInMode(t, media.AddressCloudflare)
	cover := f.image(t, media.PurposeEventCover, "images/cf-cover", 1600, 1200, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
	})
	created := f.event(t, "Cloudflare", &cover.ID)

	var got sizedEventView
	f.get(t, "/v1/events/"+created.ID.String(), &got)

	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/cdn-cgi/image/width=400,height=400,fit=scale-down/images/cf-cover", Width: 400, Height: 300},
		media.ImageAddress{URL: sizesBase + "/cdn-cgi/image/width=1200,height=1200,fit=scale-down/images/cf-cover", Width: 1200, Height: 900},
	)
	if !reflect.DeepEqual(got.CoverImageSizes, want) {
		t.Fatalf("coverImageSizes %+v, want %+v", got.CoverImageSizes, want)
	}
	if got.CoverImageURL != sizesBase+"/images/cf-cover" {
		t.Fatalf("coverImageUrl %q", got.CoverImageURL)
	}
}

// The Event summary tickets, competitors and the door answer carries the
// cover's sizes too: the door's Event list is a list of Event cards. The
// summary has no base of its own (media.ConfiguredAddresses). Not parallel:
// it sets the process-wide base, and restores it.
func TestDoorEventsCarryTheCoverSizesHTTP(t *testing.T) {
	t.Cleanup(media.UsePublicBase(sizesBase))
	f := newImageSizesFixture(t)
	sized := f.image(t, media.PurposeEventCover, "images/door-cover", 1600, 1200, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
	})
	legacy := f.image(t, media.PurposeLegacy, "images/door-legacy", 0, 0, nil)
	withCover := f.event(t, "Sized", &sized.ID)
	withLegacy := f.event(t, "Legacy", &legacy.ID)
	withoutCover := f.event(t, "Bare", nil)
	app := ticketApp(t, yk(), f.events, ticket.NewMemoryStore())

	var got []struct {
		ID              uuid.UUID                     `json:"id"`
		CoverImageURL   string                        `json:"coverImageUrl"`
		CoverImageSizes map[string]media.ImageAddress `json:"coverImageSizes"`
	}
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/door/events", nil), &got)

	legacyOriginal := media.ImageAddress{URL: sizesBase + "/images/door-legacy"}
	want := map[uuid.UUID]map[string]media.ImageAddress{
		withCover.ID: bothSizes(
			media.ImageAddress{URL: sizesBase + "/images/door-cover/card.jpg", Width: 400, Height: 300},
			media.ImageAddress{URL: sizesBase + "/images/door-cover/page.jpg", Width: 1200, Height: 900},
		),
		withLegacy.ID:   bothSizes(legacyOriginal, legacyOriginal),
		withoutCover.ID: nil,
	}
	if len(got) != len(want) {
		t.Fatalf("door events %+v", got)
	}
	for _, summary := range got {
		if !reflect.DeepEqual(summary.CoverImageSizes, want[summary.ID]) {
			t.Errorf("Event %s cover %q sizes %+v, want %+v", summary.ID, summary.CoverImageURL, summary.CoverImageSizes, want[summary.ID])
		}
	}
}

// Gallery images are answered in upload order, and two uploaded at the same
// instant in id order, so the order never changes between reads.
func TestEventGalleryOrderIsStableForImagesUploadedTogetherHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	ctx := context.Background()
	later := uuid.MustParse("ffffffff-0000-4000-8000-000000000001")
	earlier := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	var ids []uuid.UUID
	for _, id := range []uuid.UUID{later, earlier} {
		m, err := f.media.Create(ctx, media.Media{
			ID: id, Name: "photo.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "images/" + id.String(),
			UploadedBy: f.uploader, Purpose: media.PurposeEventGallery,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE media SET created_at = '2026-09-27T10:00:00Z' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	created := f.event(t, "Together", nil, later, earlier)
	// Read without index scans: an index would hand the images over in id
	// order by chance, and only the ORDER BY must decide.
	config := f.pool.Config()
	config.ConnConfig.RuntimeParams["enable_indexscan"] = "off"
	config.ConnConfig.RuntimeParams["enable_bitmapscan"] = "off"
	config.ConnConfig.RuntimeParams["enable_indexonlyscan"] = "off"
	unindexed, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unindexed.Close)
	app := eventServiceApp(t, authn.Identity{}, event.NewService(event.NewPostgresStore(unindexed), authz.NewAuthorizer(authz.DefaultPolicy()), sizesBase))

	var got sizedEventView
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/events/"+created.ID.String(), nil), &got)

	if len(got.Images) != 2 || got.Images[0].ID != earlier || got.Images[1].ID != later {
		t.Fatalf("gallery order %+v, want %s then %s", got.Images, earlier, later)
	}
	if got.ImageURLs[0] != got.Images[0].URL {
		t.Fatalf("imageUrls %v in another order than images %+v", got.ImageURLs, got.Images)
	}
}

// A picture whose object is purged while the profile still links it keeps
// answering profilePictureUrl (as before sizes existed) but has no sizes: a
// Media with no public address never gets any.
func TestMePurgedProfilePictureAnswersNoSizesHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	picture := f.image(t, media.PurposeProfilePicture, "images/purged-portrait", 1600, 1600, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 400), media.SizePage: jpegSize(1200, 1200),
	})
	app, _ := f.profile(t, &picture)
	if _, err := f.pool.Exec(context.Background(), `UPDATE media SET deleted_at = now(), blob_purge_started_at = now() WHERE id = $1`, picture.ID); err != nil {
		t.Fatal(err)
	}

	var got sizedProfileView
	answer(t, app, httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil), &got)

	if got.ProfilePictureURL != sizesBase+"/images/purged-portrait" || got.ProfilePictureSizes != nil {
		t.Fatalf("purged picture answered %q with sizes %+v", got.ProfilePictureURL, got.ProfilePictureSizes)
	}
}

// sizedVideosView is an Event detail's videos with their posters.
type sizedVideosView struct {
	Videos []struct {
		ID     uuid.UUID `json:"id"`
		Poster *struct {
			ID     uuid.UUID                     `json:"id"`
			Type   string                        `json:"type"`
			URL    string                        `json:"url"`
			Sizes  map[string]media.ImageAddress `json:"sizes"`
			Source string                        `json:"source"`
		} `json:"poster"`
	} `json:"videos"`
}

// posterFixture is an Event with two videos and the poster photo each
// gets.
func (f imageSizesFixture) posterFixture(t *testing.T) (event.Event, [2]uuid.UUID, [2]media.Media) {
	t.Helper()
	created := f.event(t, "Posters", nil)
	videos := [2]uuid.UUID{f.download(t, media.PurposeVideo).ID, f.download(t, media.PurposeVideo).ID}
	if _, err := f.events.AddFiles(context.Background(), created.ID, event.Videos, videos[:]); err != nil {
		t.Fatal(err)
	}
	posters := [2]media.Media{
		f.image(t, media.PurposeEventCover, "images/poster-cover", 1600, 1200, map[string]media.SizeObject{
			media.SizeCard: jpegSize(400, 300), media.SizePage: jpegSize(1200, 900),
		}),
		f.image(t, media.PurposeEventGallery, "images/poster-small", 800, 600, map[string]media.SizeObject{
			media.SizeCard: jpegSize(400, 300),
		}),
	}
	return created, videos, posters
}

// An Event's detail answers each video's poster with its card and page
// sizes, from the same builder as the cover's (a stored size, or the
// original for a size the image already fits in), and reads the posters in
// the query that reads the videos: the detail takes as many queries with
// posters as without.
func TestEventDetailAnswersVideoPostersWithoutAnotherQueryHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	ctx := context.Background()
	created, videos, posters := f.posterFixture(t)
	detail := func() (sizedVideosView, int64) {
		t.Helper()
		var got sizedVideosView
		f.queries.n.Store(0)
		f.get(t, "/v1/events/"+created.ID.String(), &got)
		return got, f.queries.n.Load()
	}
	bare, bareQueries := detail()
	if len(bare.Videos) != 2 || bare.Videos[0].Poster != nil || bare.Videos[1].Poster != nil {
		t.Fatalf("videos without posters %+v", bare.Videos)
	}
	for i := range videos {
		if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[i], &posters[i].ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, queries := detail()
	if queries != bareQueries {
		t.Fatalf("the detail took %d queries with posters, %d without", queries, bareQueries)
	}
	want := map[uuid.UUID]map[string]media.ImageAddress{
		posters[0].ID: bothSizes(
			media.ImageAddress{URL: sizesBase + "/images/poster-cover/card.jpg", Width: 400, Height: 300},
			media.ImageAddress{URL: sizesBase + "/images/poster-cover/page.jpg", Width: 1200, Height: 900},
		),
		posters[1].ID: bothSizes(
			media.ImageAddress{URL: sizesBase + "/images/poster-small/card.jpg", Width: 400, Height: 300},
			media.ImageAddress{URL: sizesBase + "/images/poster-small", Width: 800, Height: 600},
		),
	}
	for i, video := range got.Videos {
		if video.ID != videos[i] || video.Poster == nil || video.Poster.ID != posters[i].ID || video.Poster.Type != "image/jpeg" || video.Poster.Source != "uploaded" ||
			video.Poster.URL != sizesBase+"/"+posters[i].Key || !reflect.DeepEqual(video.Poster.Sizes, want[posters[i].ID]) {
			t.Errorf("video %d answered %+v, want the poster %s at %v", i, video, posters[i].ID, want[posters[i].ID])
		}
	}
}

// With Cloudflare image transformations configured, a poster's sizes point
// where the cover's do.
func TestVideoPosterSizesFollowTheConfiguredAddressModeHTTP(t *testing.T) {
	f := newImageSizesFixtureInMode(t, media.AddressCloudflare)
	created, videos, posters := f.posterFixture(t)
	if _, err := f.events.SetVideoPoster(context.Background(), created.ID, videos[0], &posters[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	var got sizedVideosView
	f.get(t, "/v1/events/"+created.ID.String(), &got)
	want := bothSizes(
		media.ImageAddress{URL: sizesBase + "/cdn-cgi/image/width=400,height=400,fit=scale-down/images/poster-cover", Width: 400, Height: 300},
		media.ImageAddress{URL: sizesBase + "/cdn-cgi/image/width=1200,height=1200,fit=scale-down/images/poster-cover", Width: 1200, Height: 900},
	)
	if poster := got.Videos[0].Poster; poster == nil || poster.URL != sizesBase+"/images/poster-cover" || !reflect.DeepEqual(poster.Sizes, want) {
		t.Fatalf("poster %+v, want sizes %+v", poster, want)
	}
}

// The store writes a poster only over the one it was decided against, read
// under the Event's lock: a write decided against a poster the video no
// longer has is a conflict and changes nothing. Clearing needs no decision.
func TestVideoPosterIsWrittenOnlyOverThePosterItWasDecidedAgainst(t *testing.T) {
	f := newImageSizesFixture(t)
	ctx := context.Background()
	created, videos, posters := f.posterFixture(t)
	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], &posters[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], &posters[1].ID, nil); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("a poster decided against none, over one: %v, want a conflict", err)
	}
	var got sizedVideosView
	f.get(t, "/v1/events/"+created.ID.String(), &got)
	if got.Videos[0].Poster == nil || got.Videos[0].Poster.ID != posters[0].ID {
		t.Fatalf("after the conflict the poster is %+v, want %s", got.Videos[0].Poster, posters[0].ID)
	}
	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], &posters[1].ID, &posters[0].ID); err != nil {
		t.Fatalf("a poster decided against the current one: %v", err)
	}
	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], nil, nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}

// setFrame gives the video the frame core took of it, as the frame worker
// does (event_videos.frame_media_id).
func (f imageSizesFixture) setFrame(t *testing.T, eventID, videoID, frameID uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE event_videos SET frame_media_id = $3 WHERE event_id = $1 AND media_id = $2`, eventID, videoID, frameID); err != nil {
		t.Fatal(err)
	}
}

// A video its organizers gave no poster answers the frame core took of it
// (media redesign ticket 25), in the poster's shape with source "frame",
// read in the query that reads the videos. An uploaded poster wins
// (source "uploaded") while it can be served; clearing it, or it no longer
// being servable, falls back to the frame, which stayed on the video. A
// frame that cannot be served is no poster.
func TestEventDetailAnswersAVideosFrameWhileNoPosterIsUploadedHTTP(t *testing.T) {
	f := newImageSizesFixture(t)
	ctx := context.Background()
	created, videos, posters := f.posterFixture(t)
	type answered struct {
		id     uuid.UUID
		source string
		url    string
		sizes  map[string]media.ImageAddress
	}
	detail := func() (answered, int64) {
		t.Helper()
		var got sizedVideosView
		f.queries.n.Store(0)
		f.get(t, "/v1/events/"+created.ID.String(), &got)
		if got.Videos[0].ID != videos[0] {
			t.Fatalf("videos %+v", got.Videos)
		}
		poster := got.Videos[0].Poster
		if poster == nil {
			return answered{}, f.queries.n.Load()
		}
		if poster.Type != "image/jpeg" {
			t.Fatalf("poster type %q", poster.Type)
		}
		return answered{id: poster.ID, source: poster.Source, url: poster.URL, sizes: poster.Sizes}, f.queries.n.Load()
	}
	_, bareQueries := detail()

	frame := f.image(t, media.PurposeVideoFrame, "images/frame-a", 1280, 720, map[string]media.SizeObject{
		media.SizeCard: jpegSize(400, 225), media.SizePage: jpegSize(1200, 675),
	})
	f.setFrame(t, created.ID, videos[0], frame.ID)
	wantFrame := answered{id: frame.ID, source: "frame", url: sizesBase + "/images/frame-a", sizes: bothSizes(
		media.ImageAddress{URL: sizesBase + "/images/frame-a/card.jpg", Width: 400, Height: 225},
		media.ImageAddress{URL: sizesBase + "/images/frame-a/page.jpg", Width: 1200, Height: 675},
	)}
	if got, queries := detail(); !reflect.DeepEqual(got, wantFrame) || queries != bareQueries {
		t.Fatalf("with a frame: %+v in %d queries, want %+v in %d", got, queries, wantFrame, bareQueries)
	}

	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], &posters[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := detail(); got.id != posters[0].ID || got.source != "uploaded" {
		t.Fatalf("with an uploaded poster: %+v", got)
	}
	var frameStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM media WHERE id = $1`, frame.ID).Scan(&frameStatus); err != nil || frameStatus != "attached" {
		t.Fatalf("the frame under an uploaded poster is %q (%v), want attached", frameStatus, err)
	}

	// The uploaded poster archived: the frame shows again.
	if _, err := f.pool.Exec(ctx, `UPDATE media SET deleted_at = now() WHERE id = $1`, posters[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := detail(); !reflect.DeepEqual(got, wantFrame) {
		t.Fatalf("with an archived uploaded poster: %+v", got)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE media SET deleted_at = NULL WHERE id = $1`, posters[0].ID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.events.SetVideoPoster(ctx, created.ID, videos[0], nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := detail(); !reflect.DeepEqual(got, wantFrame) {
		t.Fatalf("after the uploaded poster was cleared: %+v", got)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE media SET blob_purge_started_at = now() WHERE id = $1`, frame.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := detail(); got.id != uuid.Nil {
		t.Fatalf("a frame being purged answered %+v", got)
	}
}
