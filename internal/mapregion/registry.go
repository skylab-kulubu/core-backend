package mapregion

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mapimage"
)

// registryTTL is how long the list of ready regions is kept before it is
// read again.
const registryTTL = time.Minute

// ReadyLister lists the ready regions, the smallest box first (Store).
type ReadyLister interface {
	ListReady(ctx context.Context) ([]Region, error)
}

// Registry draws a point's map from the smallest ready region that holds
// it (and, when set, the archive at Static, MAP_TILES_URL, after every
// region). Each archive is opened once and kept; a region's object is
// never overwritten, so its directory never goes stale.
type Registry struct {
	Regions ReadyLister
	Shot    mapimage.Shooter
	// Address is a region's object's public address.
	Address func(key string) string
	// Static is an archive drawn from after every region; "" for none.
	Static string
	// Source opens an archive's address; nil reads it over HTTP.
	Source func(url string) mapimage.RangeSource

	mu        sync.Mutex
	loadedAt  time.Time
	ready     []Region
	renderers map[string]*mapimage.Renderer
}

func (r *Registry) regions(ctx context.Context, fresh bool) ([]Region, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !fresh && r.ready != nil && time.Since(r.loadedAt) < registryTTL {
		return r.ready, nil
	}
	ready, err := r.Regions.ListReady(ctx)
	if err != nil {
		return nil, err
	}
	r.ready, r.loadedAt = ready, time.Now()
	return ready, nil
}

func (r *Registry) renderer(url string) *mapimage.Renderer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.renderers == nil {
		r.renderers = map[string]*mapimage.Renderer{}
	}
	if renderer, ok := r.renderers[url]; ok {
		return renderer
	}
	var src mapimage.RangeSource = mapimage.HTTPSource{URL: url}
	if r.Source != nil {
		src = r.Source(url)
	}
	renderer := &mapimage.Renderer{Source: src, Shot: r.Shot}
	r.renderers[url] = renderer
	return renderer
}

// Map is the JPEG of the map around the point, from the smallest ready
// region holding it, else from Static. With no region and no Static,
// nothing can be drawn yet (mapimage.ErrUnavailable: no try counted, the
// worker waits); with regions none of which holds the point,
// mapimage.ErrNoMap (the Event is drawn again once a region holding it is
// ready).
func (r *Registry) Map(ctx context.Context, lat, lng float64) ([]byte, error) {
	// A list kept from before a region became ready may lack it: a point
	// none of the kept regions holds reads the list again before it says so.
	for _, fresh := range []bool{false, true} {
		regions, err := r.regions(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("%w: the regions cannot be read", mapimage.ErrUnavailable)
		}
		for _, region := range regions {
			if region.Box.Contains(lat, lng) && region.ObjectKey != "" {
				return r.renderer(r.Address(region.ObjectKey)).Map(ctx, lat, lng)
			}
		}
		if !fresh {
			continue
		}
		if r.Static != "" {
			return r.renderer(r.Static).Map(ctx, lat, lng)
		}
		if len(regions) == 0 {
			return nil, fmt.Errorf("%w: no map region is ready yet", mapimage.ErrUnavailable)
		}
	}
	return nil, mapimage.ErrNoMap
}
