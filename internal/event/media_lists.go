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
}

var mediaListSpecs = map[MediaList]mediaListSpec{
	Files:  {role: media.RoleEventFile, table: "event_files", items: func(e *Event) *[]MediaItem { return &e.Files }},
	Videos: {role: media.RoleEventVideo, table: "event_videos", items: func(e *Event) *[]MediaItem { return &e.Videos }},
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

// withItemAddresses answers each item's key at its public address.
func withItemAddresses(items []MediaItem, addresses media.Addresses) []MediaItem {
	if items == nil {
		return nil
	}
	out := make([]MediaItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].URL = addresses.Object(out[i].URL)
	}
	return out
}
