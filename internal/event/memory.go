package event

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type MemoryStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]Event
	days     map[uuid.UUID]Day
	sessions map[uuid.UUID]Session
	// media is where an Event's files and videos are read from; nil keeps
	// each item its id alone.
	media MediaReader
	// hasTickets answers whether an Event has a Ticket, what Unpublish
	// refuses on; nil is none.
	hasTickets func(eventID uuid.UUID) bool
	// hasCompetitors answers whether an Event has a current Competitor,
	// what Unpublish refuses on too; nil is none.
	hasCompetitors func(eventID uuid.UUID) bool
	// statusMails are the queued status mails (status_mail.go).
	statusMails []*memoryStatusMail
}

// ReadTicketsFrom makes Unpublish ask hasTickets whether an Event has a
// Ticket, as the database looks at its tickets table.
func (s *MemoryStore) ReadTicketsFrom(hasTickets func(eventID uuid.UUID) bool) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasTickets = hasTickets
	return s
}

// ReadCompetitorsFrom makes Unpublish ask hasCompetitors whether an Event
// has a current Competitor, as the database looks at its competitors table.
func (s *MemoryStore) ReadCompetitorsFrom(hasCompetitors func(eventID uuid.UUID) bool) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasCompetitors = hasCompetitors
	return s
}

// MediaReader reads the Media an Event's files and videos link: the Media
// store.
type MediaReader interface {
	GetIncludingDeleted(ctx context.Context, id uuid.UUID) (media.Media, error)
}

// ReadMediaFrom makes the store answer its Events' files and videos from the
// Media r holds, as the database joins them: an archived one left out, an
// address only for one that can be served (media.Media.Servable), and
// counts of those alone. Without it an item is its id alone, which only the
// Event's editors see and nothing counts.
func (s *MemoryStore) ReadMediaFrom(r MediaReader) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.media = r
	return s
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:     make(map[uuid.UUID]Event),
		days:     make(map[uuid.UUID]Day),
		sessions: make(map[uuid.UUID]Session),
	}
}

func (s *MemoryStore) List(_ context.Context, ownerTeam string, activeOnly bool) ([]Event, error) {
	return s.list(ownerTeam, activeOnly, false, lifecycle.CurrentOnly), nil
}

func (s *MemoryStore) ListIncludingDrafts(_ context.Context, ownerTeam string) ([]Event, error) {
	return s.list(ownerTeam, false, true, lifecycle.CurrentOnly), nil
}

func (s *MemoryStore) ListLifecycle(_ context.Context, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error) {
	return s.list(ownerTeam, false, true, visibility), nil
}

func (s *MemoryStore) list(ownerTeam string, activeOnly, drafts bool, visibility lifecycle.Visibility) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.byID))
	for _, e := range s.byID {
		if !visibility.Matches(e.ArchivedAt != nil) {
			continue
		}
		if !drafts && e.Status == StatusDraft {
			continue
		}
		if ownerTeam != "" && e.OwnerTeam != ownerTeam {
			continue
		}
		if activeOnly && !e.ActiveAt(time.Now()) {
			continue
		}
		out = append(out, s.listed(e))
	}
	return out
}

// listed is the Event as a list answers it: its files and videos counted,
// not listed.
func (s *MemoryStore) listed(e Event) Event {
	e = s.detailed(e)
	e.Files, e.Videos = nil, nil
	return e
}

// detailed is the Event as its detail answers it: its files and videos
// listed as the database reads them (empty lists, never nil), and counted.
func (s *MemoryStore) detailed(e Event) Event {
	e = emptyGallery(e)
	now := time.Now()
	e.Active = e.ActiveAt(now)
	e.RegistrationOpen = e.RegistrationOpenAt(now)
	e.Files, e.Videos = s.shown(e.Files), s.shown(e.Videos)
	e.FileCount, e.VideoCount = len(servableItems(e.Files)), len(servableItems(e.Videos))
	return e
}

// shown are the stored items (ids in their order) as they are shown: each
// with its Media, an address only when it can be served, a scan result only
// when rejected; an item whose Media is archived is left out.
func (s *MemoryStore) shown(stored []MediaItem) []MediaItem {
	out := make([]MediaItem, 0, len(stored))
	for _, item := range stored {
		if s.media == nil {
			out = append(out, MediaItem{ID: item.ID, poster: item.poster})
			continue
		}
		m, err := s.media.GetIncludingDeleted(context.Background(), item.ID)
		if err != nil || m.DeletedAt != nil {
			continue
		}
		shown := MediaItem{ID: m.ID, Name: m.Name, Type: m.Type, Size: m.Size, Status: m.Status}
		if m.Servable() {
			shown.URL = m.Key
		}
		if m.Status == media.StatusRejected {
			shown.ScanResult = m.ScanResult
		}
		if item.poster != nil {
			shown.poster = &linkedPoster{id: item.poster.id}
			if poster, err := s.media.GetIncludingDeleted(context.Background(), item.poster.id); err == nil && poster.DeletedAt == nil {
				shown.poster.image = media.LinkedImageOf(poster)
				if poster.Servable() {
					shown.poster.key = poster.Key
				}
			}
		}
		out = append(out, shown)
	}
	return out
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	return s.detailed(e), nil
}

func (s *MemoryStore) GetIncludingArchived(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return Event{}, ErrNotFound
	}
	return s.detailed(e), nil
}

func (s *MemoryStore) Create(ctx context.Context, e Event) (Event, error) {
	// The one-place rule again, before the store's lock: its check reads
	// the store.
	if e.formGuard != nil {
		if err := e.formGuard.check(ctx); err != nil {
			return Event{}, err
		}
		e.formGuard = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.AttendanceRule == "" {
		e.AttendanceRule = "none"
	}
	if e.Status == "" {
		e.Status = StatusPublished
	}
	e.FeedbackMode = feedbackMode(e.FeedbackMode)
	now := time.Now().UTC()
	e.RegistrationClosedAt = nil
	if e.RegistrationClosed {
		e.RegistrationClosedAt = &now
	}
	if e.Status == StatusPublished && e.PublishedAt == nil {
		e.PublishedAt = &now
	}
	e.CreatedAt = now
	e.UpdatedAt = now
	e = emptyGallery(e)
	s.byID[e.ID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) Update(ctx context.Context, e Event) (Event, error) {
	if e.formGuard != nil {
		if err := e.formGuard.check(ctx); err != nil {
			return Event{}, err
		}
		e.formGuard = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[e.ID]
	if !ok || existing.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	e.CreatedAt = existing.CreatedAt
	e.UpdatedAt = time.Now().UTC()
	// An update writes neither the status nor what goes with it.
	e.Status, e.PublishedAt, e.PublishedBy = existing.Status, existing.PublishedAt, existing.PublishedBy
	reason := existing.CancelReason
	if e.cancelReasonSent && existing.Status == StatusCancelled {
		reason = e.CancelReason
	}
	e.CancelledAt, e.CancelledBy, e.CancelReason, e.cancelReasonSent = existing.CancelledAt, existing.CancelledBy, reason, false
	e.CopiedFromEventID, e.CancelNotifiedAt = existing.CopiedFromEventID, existing.CancelNotifiedAt
	// When applying was closed by hand, as the database writes it.
	switch {
	case !e.RegistrationClosed:
		e.RegistrationClosedAt = nil
	case existing.RegistrationClosed:
		e.RegistrationClosedAt = existing.RegistrationClosedAt
	default:
		at := e.UpdatedAt
		e.RegistrationClosedAt = &at
	}
	e.Images = existing.Images
	e.ImageURLs = existing.ImageURLs
	e.Files, e.Videos = existing.Files, existing.Videos
	if e.MailListID == nil {
		e.MailListID = existing.MailListID
	}
	e = emptyGallery(e)
	s.byID[e.ID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) Archive(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if e.ArchivedAt == nil {
		now := time.Now().UTC()
		e.ArchivedAt = &now
		e.ArchivedBy = actorID
		e.UpdatedAt = now
		s.byID[id] = e
	}
	return nil
}

func (s *MemoryStore) Restore(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if e.ArchivedAt != nil {
		e.ArchivedAt = nil
		e.ArchivedBy = nil
		e.UpdatedAt = time.Now().UTC()
		s.byID[id] = e
	}
	return nil
}

func (s *MemoryStore) AddImages(_ context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	seen := map[uuid.UUID]struct{}{}
	for _, im := range e.Images {
		seen[im.ID] = struct{}{}
	}
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		e.Images = append(e.Images, GalleryImage{ID: id})
	}
	e.ImageURLs = urlsOf(e.Images)
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) RemoveImages(_ context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	have := map[uuid.UUID]GalleryImage{}
	for _, im := range e.Images {
		have[im.ID] = im
	}
	for _, id := range ids {
		if _, ok := have[id]; !ok {
			return Event{}, ErrNotFound
		}
	}
	drop := map[uuid.UUID]struct{}{}
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	kept := make([]GalleryImage, 0, len(e.Images))
	for _, im := range e.Images {
		if _, skip := drop[im.ID]; skip {
			continue
		}
		kept = append(kept, im)
	}
	e.Images = kept
	e.ImageURLs = urlsOf(e.Images)
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) ListFiles(_ context.Context, eventID uuid.UUID) ([]MediaItem, []MediaItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return nil, nil, ErrNotFound
	}
	e = s.detailed(e)
	return e.Files, e.Videos, nil
}

// AddFiles appends the Media to the Event's list. The memory store keeps no
// Media: an item is only its id, attached.
func (s *MemoryStore) AddFiles(_ context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error) {
	if !list.known() {
		return Event{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	items := e.list(list)
	for _, id := range ids {
		if id == uuid.Nil || slices.ContainsFunc(items, func(item MediaItem) bool { return item.ID == id }) {
			continue
		}
		items = append(items, MediaItem{ID: id})
	}
	e = e.withList(list, items)
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) RemoveFiles(_ context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error) {
	if !list.known() {
		return Event{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	items := e.list(list)
	for _, id := range ids {
		if !slices.ContainsFunc(items, func(item MediaItem) bool { return item.ID == id }) {
			return Event{}, ErrNotFound
		}
	}
	kept := make([]MediaItem, 0, len(items))
	for _, item := range items {
		if !slices.Contains(ids, item.ID) {
			kept = append(kept, item)
		}
	}
	e = e.withList(list, kept)
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) OrderFiles(_ context.Context, eventID uuid.UUID, list MediaList, ids []uuid.UUID) (Event, error) {
	if !list.known() || len(distinctIDs(ids)) != len(ids) {
		return Event{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	// The list as it is shown: an item whose Media is archived is not in
	// it, and keeps its place after the others.
	stored, shown := e.list(list), s.shown(e.list(list))
	if len(shown) != len(ids) || slices.ContainsFunc(shown, func(item MediaItem) bool { return !slices.Contains(ids, item.ID) }) {
		return Event{}, ErrConflict
	}
	ordered := make([]MediaItem, 0, len(stored))
	for _, id := range ids {
		item, _ := e.item(list, id)
		ordered = append(ordered, item)
	}
	for _, item := range stored {
		if !slices.Contains(ids, item.ID) {
			ordered = append(ordered, item)
		}
	}
	e = e.withList(list, ordered)
	s.byID[eventID] = e
	return s.detailed(e), nil
}

// SetVideoPoster links the poster to the Event's video, or clears it
// (nil), while its poster is still was. The memory store keeps no Media
// attachments.
func (s *MemoryStore) SetVideoPoster(_ context.Context, eventID, videoID uuid.UUID, posterID, was *uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	// The video as the list shows it: one whose Media is archived is not in
	// it.
	video, listed := Event{Videos: s.shown(e.Videos)}.item(Videos, videoID)
	if !listed {
		return Event{}, ErrNotFound
	}
	if posterID != nil && !sameID(video.posterID(), was) {
		return Event{}, ErrConflict
	}
	videos := slices.Clone(e.Videos)
	for i := range videos {
		if videos[i].ID == videoID {
			videos[i].poster = nil
			if posterID != nil {
				videos[i].poster = &linkedPoster{id: *posterID}
			}
		}
	}
	e.Videos = videos
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) TeamsUsingMedia(_ context.Context, mediaID, except uuid.UUID) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	teams := make([]string, 0)
	for _, e := range s.byID {
		if e.ID == except || slices.Contains(teams, e.OwnerTeam) {
			continue
		}
		uses := e.CoverImageID != nil && *e.CoverImageID == mediaID
		for _, im := range e.Images {
			uses = uses || im.ID == mediaID
		}
		for _, item := range slices.Concat(e.Files, e.Videos) {
			uses = uses || item.ID == mediaID || sameID(item.posterID(), &mediaID)
		}
		if uses {
			teams = append(teams, e.OwnerTeam)
		}
	}
	return teams, nil
}

func urlsOf(images []GalleryImage) []string {
	out := make([]string, 0, len(images))
	for _, im := range images {
		if im.URL != "" {
			out = append(out, im.URL)
		}
	}
	return out
}

func (s *MemoryStore) GetDay(_ context.Context, id uuid.UUID) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok || d.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	e, ok := s.byID[d.EventID]
	if !ok || e.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	return d, nil
}

func (s *MemoryStore) GetDayIncludingArchived(_ context.Context, id uuid.UUID) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return Day{}, ErrNotFound
	}
	return d, nil
}

func (s *MemoryStore) CreateDay(_ context.Context, d Day) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	s.days[d.ID] = d
	return d, nil
}

func (s *MemoryStore) ListDays(_ context.Context, eventID uuid.UUID) ([]Day, error) {
	return s.listDays(eventID, lifecycle.CurrentOnly, true), nil
}

func (s *MemoryStore) ListDaysLifecycle(_ context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error) {
	return s.listDays(eventID, visibility, false), nil
}

func (s *MemoryStore) listDays(eventID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParent bool) []Day {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Day, 0)
	if requireCurrentParent {
		e, ok := s.byID[eventID]
		if !ok || e.ArchivedAt != nil {
			return out
		}
	}
	for _, d := range s.days {
		if !visibility.Matches(d.ArchivedAt != nil) {
			continue
		}
		if d.EventID == eventID {
			out = append(out, d)
		}
	}
	return out
}

func (s *MemoryStore) UpdateDay(_ context.Context, d Day) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.days[d.ID]
	if !ok || existing.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	s.days[d.ID] = d
	return d, nil
}

func (s *MemoryStore) ArchiveDay(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return ErrNotFound
	}
	if d.ArchivedAt == nil {
		now := time.Now().UTC()
		d.ArchivedAt = &now
		d.ArchivedBy = actorID
		s.days[id] = d
	}
	return nil
}

func (s *MemoryStore) RestoreDay(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return ErrNotFound
	}
	if d.ArchivedAt != nil {
		d.ArchivedAt = nil
		d.ArchivedBy = nil
		s.days[id] = d
	}
	return nil
}

func (s *MemoryStore) ListBySeason(_ context.Context, seasonID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0)
	for _, e := range s.byID {
		if e.ArchivedAt == nil && e.Status != StatusDraft && e.SeasonID != nil && *e.SeasonID == seasonID {
			out = append(out, s.listed(e))
		}
	}
	return out, nil
}

func (s *MemoryStore) SetSeason(_ context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	e.SeasonID = seasonID
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) SetMailListID(_ context.Context, eventID, listID uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	id := listID
	e.MailListID = &id
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return s.detailed(e), nil
}

func (s *MemoryStore) GetSession(_ context.Context, id uuid.UUID) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || sess.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	d, ok := s.days[sess.EventDayID]
	if !ok || d.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	e, ok := s.byID[d.EventID]
	if !ok || e.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) GetSessionIncludingArchived(_ context.Context, id uuid.UUID) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) ListSessions(_ context.Context, eventDayID uuid.UUID) ([]Session, error) {
	return s.listSessions(eventDayID, lifecycle.CurrentOnly, true), nil
}

func (s *MemoryStore) ListSessionsLifecycle(_ context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error) {
	return s.listSessions(eventDayID, visibility, false), nil
}

func (s *MemoryStore) listSessions(eventDayID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParents bool) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0)
	if requireCurrentParents {
		d, ok := s.days[eventDayID]
		if !ok || d.ArchivedAt != nil {
			return out
		}
		e, ok := s.byID[d.EventID]
		if !ok || e.ArchivedAt != nil {
			return out
		}
	}
	for _, sess := range s.sessions {
		if !visibility.Matches(sess.ArchivedAt != nil) {
			continue
		}
		if sess.EventDayID == eventDayID {
			out = append(out, sess)
		}
	}
	return out
}

func (s *MemoryStore) CreateSession(_ context.Context, sess Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *MemoryStore) UpdateSession(_ context.Context, sess Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.sessions[sess.ID]
	if !ok || existing.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *MemoryStore) ArchiveSession(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if sess.ArchivedAt == nil {
		now := time.Now().UTC()
		sess.ArchivedAt = &now
		sess.ArchivedBy = actorID
		s.sessions[id] = sess
	}
	return nil
}

func (s *MemoryStore) RestoreSession(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if sess.ArchivedAt != nil {
		sess.ArchivedAt = nil
		sess.ArchivedBy = nil
		s.sessions[id] = sess
	}
	return nil
}

func (s *MemoryStore) Publish(_ context.Context, id uuid.UUID, by *uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || e.ArchivedAt != nil || e.Status != StatusDraft || !e.publishable() {
		return false, nil
	}
	now := time.Now().UTC()
	e.Status, e.PublishedAt, e.PublishedBy, e.UpdatedAt = StatusPublished, &now, by, now
	s.byID[id] = e
	return true, nil
}

func (s *MemoryStore) Cancel(_ context.Context, id uuid.UUID, by *uuid.UUID, reason string, notify bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	now := time.Now().UTC()
	if !ok || e.ArchivedAt != nil || e.Status != StatusPublished || e.EndedAt(now) {
		return false, nil
	}
	e.Status, e.CancelledAt, e.CancelledBy, e.CancelReason, e.UpdatedAt = StatusCancelled, &now, by, reason, now
	if notify {
		s.queueStatusMail(id, StatusMailCancelled, nil, now)
		e.CancelNotifiedAt = &now
	}
	s.byID[id] = e
	return true, nil
}

func (s *MemoryStore) Uncancel(_ context.Context, id uuid.UUID, notify bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || e.ArchivedAt != nil || e.Status != StatusCancelled {
		return false, nil
	}
	now := time.Now().UTC()
	if notify && e.CancelNotifiedAt != nil {
		var follows *memoryStatusMail
		for _, m := range s.statusMails {
			if m.EventID == id && m.Kind == StatusMailCancelled && (follows == nil || !m.CreatedAt.Before(follows.CreatedAt)) {
				follows = m
			}
		}
		if follows != nil {
			s.queueStatusMail(id, StatusMailReinstated, &follows.ID, now)
		}
	}
	e.CancelNotifiedAt = nil
	e.Status, e.CancelledAt, e.CancelledBy, e.CancelReason, e.UpdatedAt = StatusPublished, nil, nil, "", now
	s.byID[id] = e
	return true, nil
}

func (s *MemoryStore) Unpublish(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || e.ArchivedAt != nil || e.Status != StatusPublished {
		return false, nil
	}
	tickets := s.hasTickets != nil && s.hasTickets(id)
	competitors := s.hasCompetitors != nil && s.hasCompetitors(id)
	if err := unpublishRefusal(true, tickets, competitors); err != nil {
		return false, err
	}
	e.Status, e.UpdatedAt = StatusDraft, time.Now().UTC()
	s.byID[id] = e
	return true, nil
}

// Copy writes the plan's copy as the database does, without what the memory
// store does not hold: Seasons, template bindings and Competitions (a copy
// asked to bring Competitions brings none). Door staff are copied all.
func (s *MemoryStore) Copy(_ context.Context, plan CopyPlan) (Event, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	source, ok := s.byID[plan.Source]
	if !ok || source.ArchivedAt != nil {
		return Event{}, nil, ErrNotFound
	}
	if _, taken := s.byID[plan.Event.ID]; taken {
		return Event{}, nil, ErrConflict
	}
	skippedParts := map[string]bool{}
	e := plan.Event
	e.Status, e.PublishedAt, e.PublishedBy = StatusDraft, nil, nil
	e.SeasonID, e.DoorStaffIDs = nil, nil
	if e.AttendanceRule == "" {
		e.AttendanceRule = "none"
	}
	switch plan.Cover {
	case CopyRefused:
		skippedParts[SkippedCover] = true
	case CopyWrite:
	default:
		e.CoverImageID = nil
	}
	switch plan.DoorStaff {
	case CopyRefused:
		skippedParts[SkippedDoorStaff] = len(source.DoorStaffIDs) > 0
	case CopyWrite:
		e.DoorStaffIDs = slices.Clone(source.DoorStaffIDs)
	}
	now := time.Now().UTC()
	e.CreatedAt, e.UpdatedAt = now, now
	e = emptyGallery(e)
	for _, day := range s.days {
		if day.EventID != source.ID || day.ArchivedAt != nil {
			continue
		}
		copied := Day{ID: uuid.New(), EventID: e.ID, Name: day.Name, StartDate: plan.Shift.At(day.StartDate), EndDate: plan.Shift.At(day.EndDate)}
		s.days[copied.ID] = copied
		for _, sess := range s.sessions {
			if sess.EventDayID != day.ID || sess.ArchivedAt != nil {
				continue
			}
			sess.ID, sess.EventDayID, sess.Cancelled = uuid.New(), copied.ID, false
			sess.StartTime, sess.EndTime = plan.Shift.At(sess.StartTime), plan.Shift.At(sess.EndTime)
			s.sessions[sess.ID] = sess
		}
	}
	s.byID[e.ID] = e
	return s.detailed(e), skipped(skippedParts), nil
}
