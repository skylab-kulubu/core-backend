package event

import (
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// FileList is one of an Event's two lists of downloads (media redesign
// ticket 22, decision C1): its files (club files, PDF or ZIP) or its videos
// (MP4). Both hold Media sent by Direct upload, in the order the Event's
// organizers give them, and core attaches each to the Event in the list's
// role.
type FileList string

const (
	// Files are an Event's downloadable files: club_file Media, linked as
	// media.RoleEventFile (table event_files).
	Files FileList = "files"
	// Videos are an Event's videos: video Media, linked as
	// media.RoleEventVideo (table event_videos).
	Videos FileList = "videos"
)

// Role is the Media attachment role of the list's links.
func (l FileList) Role() media.Role {
	if l == Videos {
		return media.RoleEventVideo
	}
	return media.RoleEventFile
}

// table is the list's link table (migration 20260928160000); a trusted
// identifier, never input.
func (l FileList) table() string {
	if l == Videos {
		return "event_videos"
	}
	return "event_files"
}

// known reports whether l is one of the two lists.
func (l FileList) known() bool {
	return l == Files || l == Videos
}

// EventFile is one item of an Event's files or videos.
type EventFile struct {
	// ID is the Media's id.
	ID uuid.UUID `json:"id"`
	// Name is the file's name, which its download carries; empty once its
	// uploader's account was erased.
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Status is the Media's: attached once it can be served, scanning while
	// its malware scan runs, rejected once the scan rejected it.
	Status media.Status `json:"status"`
	// URL is its public address, only while it can be served: never while
	// it waits for its malware scan or once the scan rejected it (the key
	// of a held file is known only to core).
	URL string `json:"url,omitempty"`
	// ScanResult is why the malware scan rejected it: only on a rejected
	// item, which only the Event's organizers see.
	ScanResult media.ScanResult `json:"scanResult,omitempty"`
}

// downloadable reports whether anyone may see the item: its Media is clean
// and has its address.
func (f EventFile) downloadable() bool {
	return f.Status != media.StatusScanning && f.Status != media.StatusRejected && f.URL != ""
}

// list is the Event's list l, as the store read it.
func (e Event) list(l FileList) []EventFile {
	if l == Videos {
		return e.Videos
	}
	return e.Files
}

// withList is the Event with its list l replaced by items.
func (e Event) withList(l FileList, items []EventFile) Event {
	if l == Videos {
		e.Videos = items
	} else {
		e.Files = items
	}
	return e
}

// downloadableFiles are the items anyone may see: those whose Media is clean
// and has its address. A list the answer does not carry (nil) stays nil.
func downloadableFiles(items []EventFile) []EventFile {
	if items == nil {
		return nil
	}
	out := make([]EventFile, 0, len(items))
	for _, item := range items {
		if item.downloadable() {
			out = append(out, item)
		}
	}
	return out
}

// withFileAddresses answers each item's key at its public address.
func withFileAddresses(items []EventFile, addresses media.Addresses) []EventFile {
	if items == nil {
		return nil
	}
	out := make([]EventFile, len(items))
	copy(out, items)
	for i := range out {
		out[i].URL = addresses.Object(out[i].URL)
	}
	return out
}
