package event

import (
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MapAttribution is the credit an Event's map shows under it, and
// MapAttributionURL the page it links to: the map is drawn from
// OpenStreetMap's data (ODbL). In print the address is written out (OSMF's
// attribution guidelines).
const (
	MapAttribution    = "© OpenStreetMap contributors"
	MapAttributionURL = "https://www.openstreetmap.org/copyright"
)

// LocationAddressMax bounds an Event's street address, in characters (the
// database's check says the same).
const LocationAddressMax = 300

// ErrLocationInvalid is an Event's point or address that cannot be stored:
// a latitude without its longitude (or the other way round), a point off
// the globe or at 0, 0, or an address that is too long or holds control
// characters. The panel shows it on the place's fields
// (docs/event-location.md).
var ErrLocationInvalid = fmt.Errorf("event: invalid place point or address: %w", ErrInvalid)

// locationDecimals is how many decimals of a degree core keeps: about 11 cm,
// finer than any building, and stable when the panel sends the point back.
const locationDecimals = 1e6

// normalizeLocation checks e's point and address and writes them the way
// they are stored: the point rounded to six decimals, the address trimmed.
// The point comes as a pair, both or neither; 0, 0 (off the coast of
// Africa) is what a form that lost its point sends, never a club's place.
func normalizeLocation(e Event) (Event, error) {
	if (e.LocationLat == nil) != (e.LocationLng == nil) {
		return Event{}, ErrLocationInvalid
	}
	if e.LocationLat != nil {
		lat, lng := *e.LocationLat, *e.LocationLng
		if math.IsNaN(lat) || math.IsNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180 || (lat == 0 && lng == 0) {
			return Event{}, ErrLocationInvalid
		}
		lat, lng = math.Round(lat*locationDecimals)/locationDecimals, math.Round(lng*locationDecimals)/locationDecimals
		e.LocationLat, e.LocationLng = &lat, &lng
	}
	e.LocationAddress = strings.TrimSpace(e.LocationAddress)
	if utf8.RuneCountInString(e.LocationAddress) > LocationAddressMax || !utf8.ValidString(e.LocationAddress) ||
		strings.IndexFunc(e.LocationAddress, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) >= 0 {
		return Event{}, ErrLocationInvalid
	}
	return e, nil
}

// HasPoint reports whether the Event's place has a point on the map.
func (e Event) HasPoint() bool {
	return e.LocationLat != nil && e.LocationLng != nil
}
