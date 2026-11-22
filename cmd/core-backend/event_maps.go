package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/mapregion"
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

// startEventMaps starts the Event map worker and the map regions' worker
// (docs/event-map.md). An Event whose place has a point gets the map around
// it, drawn once by Gotenberg from the smallest ready map region holding it
// (or from MAP_TILES_URL's archive, after every region). The regions'
// worker extracts the regions an ADMIN asks for from the latest Protomaps
// build into the public bucket. Both need R2; the maps need Gotenberg too.
// The log names hosts only.
func startEventMaps(ctx context.Context, worker func(string, <-chan struct{}), render certificate.Renderer, publicBlobs media.BlobStore,
	store *media.PostgresStore, purposes media.Catalogue, budget *media.DecodeBudget, regions *mapregion.Store) *media.MapWorker {
	tilesURL, err := mapimage.TilesURLFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	maxBytes, err := mapregion.MaxBytesFromEnv(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	gotenberg, _ := render.(*certificate.Gotenberg)
	r2, hasR2 := publicBlobs.(*media.R2)
	if !hasR2 {
		log.Printf("event maps: off (no R2 configured); Events show their place without a map")
		return nil
	}
	extractor := &mapregion.Worker{
		Store: regions, Uploader: r2, MaxBytes: maxBytes,
		Builds: mapregion.Builds{ListURL: strings.TrimSpace(os.Getenv(mapregion.BuildsURLEnv))},
	}
	worker("map regions", extractor.Run(ctx, log.Printf))
	log.Printf("map regions: on (extracts of at most %d bytes)", maxBytes)
	if gotenberg == nil {
		log.Printf("event maps: off (no Gotenberg); Events show their place without a map")
		return nil
	}
	registry := &mapregion.Registry{Regions: regions, Shot: gotenberg, Address: media.ConfiguredAddresses().Object, Static: tilesURL}
	mapWorker, err := media.NewMapWorker(media.MapWorkerConfig{
		Store: store, Storage: r2, Catalogue: purposes, DecodeBudget: budget,
		Maps: eventMaps{renderer: registry},
	})
	if err != nil {
		log.Fatal(err)
	}
	worker("event maps", mapWorker.Run(ctx, log.Printf))
	if tilesURL != "" {
		host := ""
		if u, err := url.Parse(tilesURL); err == nil {
			host = u.Host
		}
		log.Printf("event maps: on (map regions, and tiles at %s)", host)
	} else {
		log.Printf("event maps: on (map regions; none ready draws no map)")
	}
	return mapWorker
}
