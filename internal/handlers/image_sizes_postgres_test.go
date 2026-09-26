package handlers

import (
	"context"
	"encoding/json"
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
		PublicBase: sizesBase,
		Media:      media.NewLinker(mediaStore),
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

// The Event list answers each Event's cover and gallery sizes, and costs the
// same number of queries for four Events as for one: the Media each Event
// links are read with it, never one query per Event or per Media.
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
		return f.event(t, "Event "+key, &cover.ID, gallery.ID, legacy.ID), seeded{cover: cover, gallery: gallery}
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
