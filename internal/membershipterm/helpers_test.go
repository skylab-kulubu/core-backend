package membershipterm_test

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

func yk() identity.Group {
	return identity.Group{ID: gYK.String(), Name: "YK", Path: "/UYELER/YK"}
}

type nilDirectory struct{}

func (nilDirectory) ListGroups(context.Context) ([]identity.Group, error) { return nil, nil }
func (nilDirectory) GetGroup(context.Context, string) (identity.Group, error) {
	return identity.Group{}, identity.ErrNotFound
}
func (nilDirectory) Members(context.Context, string) ([]identity.Person, error) { return nil, nil }

type nilSeasons struct{}

func (nilSeasons) List(context.Context, bool) ([]season.Season, error) { return nil, nil }

type nilRosters struct{}

func (nilRosters) MemberIDs(context.Context, string) ([]uuid.UUID, error) { return nil, nil }
