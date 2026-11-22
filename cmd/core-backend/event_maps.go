package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// eventMaps is the map renderer as the map worker asks it: its errors in
// the worker's terms.
type eventMaps struct {
	renderer interface {
		Map(ctx context.Context, lat, lng float64) ([]byte, error)
	}
}

func (m eventMaps) Map(ctx context.Context, lat, lng float64) ([]byte, error) {
	picture, err := m.renderer.Map(ctx, lat, lng)
	switch {
	case err == nil:
		return picture, nil
	case errors.Is(err, mapimage.ErrNoMap):
		return nil, errors.Join(media.ErrNoEventMap, err)
	case errors.Is(err, mapimage.ErrUnavailable), errors.Is(err, mapimage.ErrSchema):
		// Nothing can be drawn now: no try is counted, the worker waits.
		return nil, errors.Join(media.ErrMapSourceDown, err)
	default:
		// This map failed (Gotenberg refused it, a tile does not parse):
		// a try is counted, and the pass goes on with the next Event.
		return nil, err
	}
}

// startEventMaps starts the Event map worker (MAP_TILES_URL,
// docs/event-map.md): an Event whose place has a point gets the map around
// it, drawn once from the club's own OpenStreetMap tiles by Gotenberg. It
// needs the tiles, Gotenberg and R2; without any of them Events show their
// place without a map. The log names the tiles' host only.
func startEventMaps(ctx context.Context, worker func(string, <-chan struct{}), render certificate.Renderer, publicBlobs media.BlobStore,
	store *media.PostgresStore, purposes media.Catalogue, budget *media.DecodeBudget) *media.MapWorker {
	tilesURL, err := mapimage.TilesURLFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	gotenberg, _ := render.(*certificate.Gotenberg)
	r2, hasR2 := publicBlobs.(*media.R2)
	switch {
	case tilesURL == "":
		log.Printf("event maps: off (%s is not set); Events show their place without a map", mapimage.TilesURLEnv)
		return nil
	case gotenberg == nil:
		log.Printf("event maps: off (no Gotenberg); Events show their place without a map")
		return nil
	case !hasR2:
		log.Printf("event maps: off (no R2 configured); Events show their place without a map")
		return nil
	}
	mapWorker, err := media.NewMapWorker(media.MapWorkerConfig{
		Store: store, Storage: r2, Catalogue: purposes, DecodeBudget: budget,
		Maps: eventMaps{renderer: &mapimage.Renderer{Source: mapimage.HTTPSource{URL: tilesURL}, Shot: gotenberg}},
	})
	if err != nil {
		log.Fatal(err)
	}
	worker("event maps", mapWorker.Run(ctx, log.Printf))
	host := ""
	if u, err := url.Parse(tilesURL); err == nil {
		host = u.Host
	}
	log.Printf("event maps: on (tiles at %s)", host)
	return mapWorker
}
