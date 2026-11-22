package ticketpage

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// maxLogoBytes bounds a team logo put in a PDF.
const maxLogoBytes = 512 << 10

// logoTTL is how long a team's logo (or its absence) is kept: a PDF does
// not ask Keycloak and the bucket every time.
const logoTTL = 10 * time.Minute

// GroupReader reads a team's group (identity.Directory).
type GroupReader interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

// MediaTeamLogos reads a team's logo from its group's logo_media_id: the
// public Media only (a private one is never put in a ticket), an image of
// at most 512 KB.
type MediaTeamLogos struct {
	Groups GroupReader
	Media  media.Store
	Blobs  media.BlobStore

	mu    sync.Mutex
	cache map[string]cachedLogo
}

type cachedLogo struct {
	data        []byte
	contentType string
	at          time.Time
}

func (m *MediaTeamLogos) TeamLogo(ctx context.Context, team string) ([]byte, string, bool) {
	if m == nil || m.Groups == nil || m.Media == nil || m.Blobs == nil || team == "" {
		return nil, "", false
	}
	m.mu.Lock()
	if hit, ok := m.cache[team]; ok && time.Since(hit.at) < logoTTL {
		m.mu.Unlock()
		return hit.data, hit.contentType, len(hit.data) > 0
	}
	m.mu.Unlock()
	data, contentType := m.read(ctx, team)
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]cachedLogo{}
	}
	m.cache[team] = cachedLogo{data: data, contentType: contentType, at: time.Now()}
	m.mu.Unlock()
	return data, contentType, len(data) > 0
}

func (m *MediaTeamLogos) read(ctx context.Context, team string) ([]byte, string) {
	group, err := m.Groups.GetGroup(ctx, team)
	if err != nil {
		return nil, ""
	}
	id, err := uuid.Parse(strings.TrimSpace(group.Attributes["logo_media_id"]))
	if err != nil {
		return nil, ""
	}
	item, err := m.Media.Get(ctx, id)
	if err != nil || item.DeletedAt != nil || !logoTypes[item.Type] || item.Size > maxLogoBytes {
		return nil, ""
	}
	if _, private := item.Sealed(); private {
		return nil, ""
	}
	data, err := m.Blobs.Read(ctx, item.Key)
	if err != nil || len(data) == 0 || len(data) > maxLogoBytes {
		return nil, ""
	}
	return data, item.Type
}
