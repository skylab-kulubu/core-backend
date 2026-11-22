package mapimage

import (
	"context"
	"errors"
)

// CheckPoint is the point a new archive is checked at: YTÜ Davutpaşa, in
// every extract the club makes.
var CheckPoint = [2]float64{41.025376, 28.889648}

// CheckReport is what a new archive gives at a known point before core
// reads it (docs/event-map.md, "Refreshing"): its zooms and bounds, the
// basemap's name and version, and what the map there drew.
type CheckReport struct {
	Zooms       [2]int     `json:"zooms"`
	Bounds      [4]float64 `json:"bounds"`
	Basemap     string     `json:"basemap"`
	Version     string     `json:"version"`
	Attribution string     `json:"attribution"`
	Point       [2]float64 `json:"point"`
	Stats       Stats      `json:"drawn"`
	SVGBytes    int        `json:"svgBytes"`
	// Problems names what is wrong; none is a map core can draw from.
	Problems []string `json:"problems"`
}

// layersRequired are the layers a map of a city point must draw: without
// any of them the archive is not what the map is drawn for.
var layersRequired = []string{"earth", "roads", "buildings"}

// Check draws the map at the point from the archive and reports it: an
// archive of another basemap or version, one that does not reach zoom 15,
// or one whose map there lacks land, roads or buildings, has problems.
func Check(ctx context.Context, src RangeSource, lat, lng float64) (CheckReport, error) {
	a, err := Open(ctx, src)
	if err != nil {
		return CheckReport{}, err
	}
	h := a.Header()
	report := CheckReport{
		Zooms: [2]int{int(h.MinZoom), int(h.MaxZoom)}, Bounds: [4]float64{h.MinLon, h.MinLat, h.MaxLon, h.MaxLat},
		Point: [2]float64{lat, lng}, Problems: []string{},
	}
	metadata, err := a.Metadata(ctx)
	if err != nil {
		return CheckReport{}, err
	}
	report.Basemap, _ = metadata["name"].(string)
	report.Attribution, _ = metadata["attribution"].(string)
	version, ok := SchemaVersion(metadata)
	report.Version = version
	if !ok {
		report.Problems = append(report.Problems, "not a Protomaps basemap of version 4")
	}
	if version == "" {
		report.Problems = append(report.Problems, "no basemap version in the metadata")
	}
	if report.Attribution == "" {
		report.Problems = append(report.Problems, "no attribution in the metadata (ODbL asks it kept)")
	}
	if h.MaxZoom < 15 {
		report.Problems = append(report.Problems, "the archive stops before zoom 15")
	}
	svg, stats, err := render(ctx, a, lat, lng, DefaultOptions)
	report.Stats, report.SVGBytes = stats, len(svg)
	switch {
	case errors.Is(err, ErrNoMap):
		report.Problems = append(report.Problems, "no map of the point")
	case err != nil:
		return report, err
	}
	for _, layer := range layersRequired {
		if stats.Features[layer] == 0 {
			report.Problems = append(report.Problems, "nothing drawn of the layer "+layer)
		}
	}
	return report, nil
}
