package mapimage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
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

// ErrShot is a picture the renderer refused or could not finish for this
// map (an error status, an answer that is not a JPEG, a request that timed
// out): a failure of the point, tried again later and given up after a few,
// so that one map Gotenberg always refuses never holds up the others.
// Gotenberg that cannot be reached at all is ErrUnavailable instead.
var ErrShot = errors.New("mapimage: the map's picture was refused")

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
	metadata, err := a.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	if version, ok := SchemaVersion(metadata); !ok {
		return nil, fmt.Errorf("%w (it says %q)", ErrSchema, version)
	}
	r.archive = a
	return a, nil
}

// unreachable reports whether a request failed before any answer: the
// address does not resolve, or nothing listens there.
func unreachable(err error) bool {
	var dns *net.DNSError
	var op *net.OpError
	return errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial") || errors.Is(err, syscall.ECONNREFUSED)
}

// Map is the JPEG of the map around the point. ErrNoMap: the archive has
// none there. ErrUnavailable or ErrSchema: nothing can be drawn now (the
// tiles or Gotenberg cannot be reached, or the archive is of another
// basemap). ErrShot: Gotenberg refused this map.
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
	switch {
	case err == nil:
		return picture, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case unreachable(err):
		return nil, fmt.Errorf("%w: Gotenberg cannot be reached", ErrUnavailable)
	default:
		return nil, fmt.Errorf("%w: %v", ErrShot, err)
	}
}
