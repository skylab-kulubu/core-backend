package mapregion

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
)

// Service is the regions as the API answers them (docs/event-map.md,
// "Regions"): every region with its state for who holds media:manage (ADMIN,
// YK), who also asks for, asks again for and retires regions; the ready
// ones alone (name, box, zoom, address) for anyone signed in, whose panel
// map reads them.
type Service struct {
	Store   *Store
	Authz   authz.Authorizer
	Address func(key string) string
}

func (s *Service) manager(p authz.Principal) bool {
	return s.Authz.Permitted(p, authz.RoleMediaManage)
}

func (s *Service) answer(r Region, full bool) Region {
	if r.Status == StatusReady && r.ObjectKey != "" {
		r.URL = s.Address(r.ObjectKey)
	}
	if !full {
		return Region{ID: r.ID, Name: r.Name, Box: r.Box, BBox: r.BBox, MaxZoom: r.MaxZoom, Status: r.Status, URL: r.URL, CreatedAt: r.CreatedAt, ReadyAt: r.ReadyAt}
	}
	return r
}

// List is every region for a manager, the ready ones for anyone else.
func (s *Service) List(ctx context.Context, p authz.Principal) ([]Region, error) {
	full := s.manager(p)
	var regions []Region
	var err error
	if full {
		regions, err = s.Store.List(ctx)
	} else {
		regions, err = s.Store.ListReady(ctx)
	}
	if err != nil {
		return nil, err
	}
	for i := range regions {
		regions[i] = s.answer(regions[i], full)
	}
	return regions, nil
}

// Request asks for a region: a manager only; ErrInvalid for a name, box or
// zoom out of bounds, ErrExists for a box a live region has.
func (s *Service) Request(ctx context.Context, p authz.Principal, name string, box mapimage.BBox, maxZoom int) (Region, error) {
	if !s.manager(p) {
		return Region{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !validRequest(name, box, maxZoom) {
		return Region{}, ErrInvalid
	}
	r, err := s.Store.Create(ctx, name, box, maxZoom)
	if err != nil {
		return Region{}, err
	}
	return s.answer(r, true), nil
}

// Retry asks again for a failed region; Retire stops using a region.
func (s *Service) Retry(ctx context.Context, p authz.Principal, id uuid.UUID) (Region, error) {
	return s.change(ctx, p, id, s.Store.Retry)
}

func (s *Service) Retire(ctx context.Context, p authz.Principal, id uuid.UUID) (Region, error) {
	return s.change(ctx, p, id, s.Store.Retire)
}

func (s *Service) change(ctx context.Context, p authz.Principal, id uuid.UUID, do func(context.Context, uuid.UUID) (Region, error)) (Region, error) {
	if !s.manager(p) {
		return Region{}, ErrForbidden
	}
	r, err := do(ctx, id)
	if err != nil {
		return Region{}, err
	}
	return s.answer(r, true), nil
}
