package handlers

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mapimage"
	"github.com/skylab-kulubu/core-backend/internal/mapregion"
)

// MapRegionHandler answers /v1/map-regions (docs/event-map.md, "Regions").
type MapRegionHandler struct {
	svc *mapregion.Service
}

func NewMapRegionHandler(svc *mapregion.Service) *MapRegionHandler {
	return &MapRegionHandler{svc: svc}
}

func mapRegionError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, mapregion.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, mapregion.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, mapregion.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "map_region_invalid")
	case errors.Is(err, mapregion.ErrExists):
		return problemCode(c, fiber.StatusConflict, "Conflict", "map_region_exists")
	case errors.Is(err, mapregion.ErrState):
		return problemCode(c, fiber.StatusConflict, "Conflict", "map_region_state")
	default:
		return err
	}
}

// List is GET /v1/map-regions.
func (h *MapRegionHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mapRegionError(c, err)
	}
	regions, err := h.svc.List(c.Context(), p)
	if err != nil {
		return mapRegionError(c, err)
	}
	return c.JSON(regions)
}

type mapRegionBody struct {
	Name    string     `json:"name"`
	BBox    *[]float64 `json:"bbox"`
	MaxZoom *int       `json:"maxZoom"`
}

// Request is POST /v1/map-regions: 202 with the region, extracted in the
// background.
func (h *MapRegionHandler) Request(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mapRegionError(c, err)
	}
	var body mapRegionBody
	if err := c.Bind().Body(&body); err != nil || body.BBox == nil || len(*body.BBox) != 4 {
		return mapRegionError(c, mapregion.ErrInvalid)
	}
	box := *body.BBox
	maxZoom := mapregion.MaxZoom
	if body.MaxZoom != nil {
		maxZoom = *body.MaxZoom
	}
	region, err := h.svc.Request(c.Context(), p, body.Name, mapimage.BBox{MinLon: box[0], MinLat: box[1], MaxLon: box[2], MaxLat: box[3]}, maxZoom)
	if err != nil {
		return mapRegionError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(region)
}

// Retry is POST /v1/map-regions/{id}/retry; Retire POST …/retire.
func (h *MapRegionHandler) Retry(c fiber.Ctx) error {
	return h.change(c, h.svc.Retry)
}

func (h *MapRegionHandler) Retire(c fiber.Ctx) error {
	return h.change(c, h.svc.Retire)
}

func (h *MapRegionHandler) change(c fiber.Ctx, do func(ctx context.Context, p authz.Principal, id uuid.UUID) (mapregion.Region, error)) error {
	p, err := caller(c)
	if err != nil {
		return mapRegionError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return mapRegionError(c, mapregion.ErrNotFound)
	}
	region, err := do(c.Context(), p, id)
	if err != nil {
		return mapRegionError(c, err)
	}
	return c.JSON(region)
}
