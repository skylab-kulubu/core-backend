package event

import (
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// MediaList is one of an Event's two lists of Media (media redesign ticket
// 22, decision C1): its files (club files, PDF or ZIP) or its videos (MP4).
// Both hold Media sent by Direct upload, in the order the Event's organizers
// give them, and core attaches each to the Event in the list's role.
type MediaList string

const (
	// Files are an Event's downloadable files: club_file Media.
	Files MediaList = "files"
	// Videos are an Event's videos: video Media.
	Videos MediaList = "videos"
)

// mediaListSpec is what tells an Event's two lists apart.
type mediaListSpec struct {
	// role is the Media attachment role of the list's links.
	role media.Role
	// table is the list's link table (migration 20260928160000); a trusted
	// identifier, never input.
	table string
	// items is the Event's field that holds the list.
	items func(e *Event) *[]MediaItem
	// posters tells whether the list's items may have a poster (a video's,
	// in event_videos.poster_media_id, migration 20260929140000).
	posters bool
}

var mediaListSpecs = map[MediaList]mediaListSpec{
	Files:  {role: media.RoleEventFile, table: "event_files", items: func(e *Event) *[]MediaItem { return &e.Files }},
	Videos: {role: media.RoleEventVideo, table: "event_videos", items: func(e *Event) *[]MediaItem { return &e.Videos }, posters: true},
}

// mediaLists are both lists, in the order a store reads them.
var mediaLists = []MediaList{Files, Videos}

// known reports whether l is one of the two lists.
func (l MediaList) known() bool {
	_, ok := mediaListSpecs[l]
	return ok
}

// Role is the Media attachment role of the list's links.
func (l MediaList) Role() media.Role { return mediaListSpecs[l].role }

func (l MediaList) table() string { return mediaListSpecs[l].table }

// posterSQL is what the list's items query selects for an item's uploaded
// poster (poster_id, poster_key, poster) and its frame (frame_id,
// frame_key, frame), and the joins it reads them through: NULLs and no join
// for a list whose items have none. Each id is the video's own link,
// archived or not; its key only while it can be served
// (media.ServableKeySQL), and its Media (media.LinkedImageSQL) only while
// it is not archived.
func (l MediaList) posterSQL() (columns, join string) {
	if !mediaListSpecs[l].posters {
		return `NULL::uuid AS poster_id, '' AS poster_key, NULL::jsonb AS poster,
			NULL::uuid AS frame_id, '' AS frame_key, NULL::jsonb AS frame`, ``
	}
	return `linked.poster_media_id AS poster_id, COALESCE(` + media.ServableKeySQL("p") + `, '') AS poster_key, ` + media.LinkedImageSQL("p") + ` AS poster,
			linked.frame_media_id AS frame_id, COALESCE(` + media.ServableKeySQL("fr") + `, '') AS frame_key, ` + media.LinkedImageSQL("fr") + ` AS frame`,
		`
		LEFT JOIN media p ON p.id = linked.poster_media_id AND p.deleted_at IS NULL
		LEFT JOIN media fr ON fr.id = linked.frame_media_id AND fr.deleted_at IS NULL`
}

// mediaLink is a Media an Event links, in its role.
type mediaLink struct {
	id   uuid.UUID
	role media.Role
}

// links are the Media an item of the list links on the Event, each in its
// role: its own, in the list's role, and a video's poster unless the
// poster's Media is archived (left out as an archived gallery photo is).
func (l MediaList) links(item MediaItem) []mediaLink {
	links := []mediaLink{{id: item.ID, role: l.Role()}}
	if item.poster != nil && item.poster.image != nil {
		links = append(links, mediaLink{id: item.poster.id, role: media.RoleEventVideoPoster})
	}
	return links
}

// MediaItem is one item of an Event's files or videos.
type MediaItem struct {
	// ID is the Media's id.
	ID uuid.UUID `json:"id"`
	// Name is the name the file was uploaded under; empty once its
	// uploader's account was erased. A ZIP downloads under it; a PDF or a
	// video opens in the browser, and saving one takes the name of its
	// address.
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Status is the Media's: attached once it can be served, scanning while
	// its malware scan runs, rejected once the scan rejected it.
	Status media.Status `json:"status"`
	// URL is its public address. The store answers one only for a Media
	// that can be served (media.ServableSQL, media.Media.Servable): never
	// while it waits for its malware scan or once the scan rejected it,
	// never for a private one, never once its object is being purged.
	URL string `json:"url,omitempty"`
	// ScanResult is why the malware scan rejected it: only on a rejected
	// item, which only the Event's organizers see.
	ScanResult media.ScanResult `json:"scanResult,omitempty"`
	// Poster is a video's poster image: the one its organizers uploaded
	// (media redesign ticket 24) while it can be served, else the frame core
	// took of the video (ticket 25) while that can be served. A file has
	// none, and neither has a video without either.
	Poster *Poster `json:"poster,omitempty"`

	// poster is the video's uploaded poster as the store read it with the
	// video; nil for a file, or a video without one.
	poster *linkedPoster
	// frame is the frame core took of the video, as the store read it; nil
	// for a file, or a video without one.
	frame *linkedPoster
}

// linkedPoster is a video's poster as the store read it with the video.
type linkedPoster struct {
	// id is the poster Media the video links, archived or not: what a new
	// poster is compared with.
	id uuid.UUID
	// key is its object key while it can be served (media.ServableKeySQL),
	// and empty otherwise.
	key string
	// image is its Media, what its type and sizes are built from; nil once
	// it is archived.
	image *media.LinkedImage
}

// Poster is a video's poster at its full-size address, with its card and
// page sizes, built like an Event cover's (media.Addresses.LinkedSizes).
// Type is the image's content type: an SVG (image/svg+xml) needs an SVG
// renderer, and its sizes have no width or height. Source says where it
// comes from: PosterUploaded or PosterFrame.
type Poster struct {
	ID     uuid.UUID                     `json:"id"`
	Type   string                        `json:"type"`
	URL    string                        `json:"url"`
	Sizes  map[string]media.ImageAddress `json:"sizes"`
	Source string                        `json:"source"`
}

// Where a video's poster comes from (Poster.Source).
const (
	// PosterUploaded: its organizers uploaded it (ticket 24).
	PosterUploaded = "uploaded"
	// PosterFrame: core took it from the video (ticket 25).
	PosterFrame = "frame"
)

// posterAt is the item's poster at its addresses: its uploaded poster while
// that can be served, else its frame while that can; nil for neither.
func (item MediaItem) posterAt(addresses media.Addresses) *Poster {
	if poster := item.poster.at(addresses, PosterUploaded); poster != nil {
		return poster
	}
	return item.frame.at(addresses, PosterFrame)
}

// at is the linked image as a poster from source: nil when there is none,
// or it cannot be served.
func (p *linkedPoster) at(addresses media.Addresses, source string) *Poster {
	if p == nil || p.key == "" {
		return nil
	}
	sizes := addresses.LinkedSizes(p.image)
	if sizes == nil {
		return nil
	}
	return &Poster{ID: p.id, Type: p.image.Type(), URL: addresses.Object(p.key), Sizes: sizes, Source: source}
}

// posterID is the poster Media the video links, archived or not; nil for
// none.
func (item MediaItem) posterID() *uuid.UUID {
	if item.poster == nil {
		return nil
	}
	return &item.poster.id
}

// item is the item of the Event's list l whose Media is id, as the store
// read it.
func (e Event) item(l MediaList, id uuid.UUID) (MediaItem, bool) {
	for _, item := range e.list(l) {
		if item.ID == id {
			return item, true
		}
	}
	return MediaItem{}, false
}

// list is the Event's list l, as the store read it; nil for a list it did
// not read, or no list.
func (e Event) list(l MediaList) []MediaItem {
	spec, ok := mediaListSpecs[l]
	if !ok {
		return nil
	}
	return *spec.items(&e)
}

// withList is the Event with its list l replaced by items.
func (e Event) withList(l MediaList, items []MediaItem) Event {
	if spec, ok := mediaListSpecs[l]; ok {
		*spec.items(&e) = items
	}
	return e
}

// servableItems are the items anyone may see: those the store answered an
// address for. A list the answer does not carry (nil) stays nil.
func servableItems(items []MediaItem) []MediaItem {
	if items == nil {
		return nil
	}
	out := make([]MediaItem, 0, len(items))
	for _, item := range items {
		if item.URL != "" {
			out = append(out, item)
		}
	}
	return out
}

// withItemAddresses answers each item's key at its public address, and a
// video's poster at its own.
func withItemAddresses(items []MediaItem, addresses media.Addresses) []MediaItem {
	if items == nil {
		return nil
	}
	out := make([]MediaItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].URL = addresses.Object(out[i].URL)
		out[i].Poster = out[i].posterAt(addresses)
	}
	return out
}

// sameID reports whether a and b name the same Media, or both none.
func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
