-- Rolls back the map regions: the list goes, the extracts in the public
-- bucket stay (MAP_TILES_URL still draws from one).
DROP TABLE IF EXISTS map_regions;
