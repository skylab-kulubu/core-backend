-- The club's map regions (docs/event-map.md, "Regions"): extracts of
-- OpenStreetMap (Protomaps basemap) the Event maps are drawn from. An
-- ADMIN asks for a region by its name and box; core extracts it from the
-- latest Protomaps build into the public bucket (maps/…), and the map of a
-- point is drawn from the smallest ready region that holds it.
--
-- A worker claims a requested region in a short transaction (claim_id,
-- claimed_until; one region at a time) and writes its progress while it
-- uploads. failed keeps why (error_code); retired is a region no longer
-- used, whose object stays. No personal data: who asked is in the log, not
-- here.
--
-- The release before this one reads and writes nothing of it.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS map_regions (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    min_lon DOUBLE PRECISION NOT NULL,
    min_lat DOUBLE PRECISION NOT NULL,
    max_lon DOUBLE PRECISION NOT NULL,
    max_lat DOUBLE PRECISION NOT NULL,
    max_zoom SMALLINT NOT NULL DEFAULT 15,
    status TEXT NOT NULL DEFAULT 'requested',
    object_key TEXT,
    source_build TEXT,
    size_bytes BIGINT,
    progress_bytes BIGINT NOT NULL DEFAULT 0,
    error_code TEXT,
    attempts INTEGER NOT NULL DEFAULT 0,
    claim_id UUID,
    claimed_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at TIMESTAMPTZ,
    CONSTRAINT map_regions_check CHECK (
        char_length(btrim(name)) BETWEEN 1 AND 80
        AND min_lon >= -180 AND max_lon <= 180 AND min_lon < max_lon
        AND min_lat >= -85.05112878 AND max_lat <= 85.05112878 AND min_lat < max_lat
        AND max_zoom BETWEEN 0 AND 15
        AND status IN ('requested', 'extracting', 'ready', 'failed', 'retired')
        AND (status <> 'ready' OR (object_key IS NOT NULL AND size_bytes IS NOT NULL AND ready_at IS NOT NULL))
        AND progress_bytes >= 0 AND attempts >= 0
        AND (claim_id IS NULL) = (claimed_until IS NULL)
    )
);

-- One live region per box: a second request for a box being extracted or
-- ready is the same region.
CREATE UNIQUE INDEX IF NOT EXISTS map_regions_live_box_idx
    ON map_regions (min_lon, min_lat, max_lon, max_lat)
    WHERE status IN ('requested', 'extracting', 'ready');

-- The worker's walk and the renderer's list.
CREATE INDEX IF NOT EXISTS map_regions_status_idx ON map_regions (status, created_at);
