package mapimage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// TilesURLEnv names the club's PMTiles archive (a Protomaps basemap extract
// of OpenStreetMap), read with HTTP range requests: an object in the public
// bucket, or a static server on the internal network. Unset, Events get no
// map (docs/event-map.md).
const TilesURLEnv = "MAP_TILES_URL"

// TilesURLFromEnv reads TilesURLEnv: empty, or an http(s) address with a
// host and no credentials.
func TilesURLFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(TilesURLEnv))
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("%s: want an http(s) address of a PMTiles archive", TilesURLEnv)
	}
	return raw, nil
}

// Shooter takes a picture of a page (core's Gotenberg).
type Shooter interface {
	Screenshot(ctx context.Context, html string, width, height, quality int) ([]byte, error)
}

// ErrShot is a picture the renderer could not take (Gotenberg down, busy,
// or refusing the page).
var ErrShot = errors.New("mapimage: the map's picture could not be taken")

// Quality is the map picture's JPEG quality: about 80 KB for 1280 × 480.
const Quality = 82

// Renderer draws an Event's map: the SVG from the archive's tiles, then its
// picture. It opens the archive on first use, and again after a failure.
type Renderer struct {
	Source  RangeSource
	Shot    Shooter
	Options Options

	mu      sync.Mutex
	archive *Archive
}

func (r *Renderer) open(ctx context.Context) (*Archive, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.archive != nil {
		return r.archive, nil
	}
	a, err := Open(ctx, r.Source)
	if err != nil {
		return nil, err
	}
	r.archive = a
	return a, nil
}

// Map is the JPEG of the map around the point. ErrNoMap: the archive has
// none there. ErrUnavailable or ErrShot: it cannot be drawn now.
func (r *Renderer) Map(ctx context.Context, lat, lng float64) ([]byte, error) {
	a, err := r.open(ctx)
	if err != nil {
		return nil, err
	}
	o := r.Options
	if o.Width == 0 {
		o = DefaultOptions
	}
	svg, err := RenderSVG(ctx, a, lat, lng, o)
	if err != nil {
		return nil, err
	}
	picture, err := r.Shot.Screenshot(ctx, PageHTML(svg), o.Width*o.Scale, o.Height*o.Scale, Quality)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShot, err)
	}
	return picture, nil
}
