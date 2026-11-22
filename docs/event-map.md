# An Event's map

When an Event's place gets a point ([event-location.md](event-location.md)),
core draws the map around it **once** and keeps it as an `event_map` Media.
The ticket PDF and the ticket page show it ([ticket-page.md](ticket-page.md)).
Yusuf's choice (2026-11-22): the club's own OpenStreetMap data, no map
service, no key.

## How it is drawn

1. **Tiles:** the club's PMTiles archive of OpenStreetMap, a
   [Protomaps basemap](https://docs.protomaps.com/basemaps/layers) extract
   (schema v4, vector tiles to zoom 15), read with HTTP range requests at
   `MAP_TILES_URL`. Core reads the header and root directory once, then the
   few tiles around the point (two to four at zoom 15) and the leaf
   directories they need.
2. **Drawing:** core draws the tiles itself as an SVG (`internal/mapimage`):
   640 × 240 CSS pixels at zoom 16, about 1.1 km across in Istanbul, in the
   ticket's greys (land, water, green areas, campuses, buildings, roads with
   their casings, footpaths dashed, railways; tunnels left out), the names
   of the larger places (universities, hospitals, parks, stations) and of
   the streets that have room for them, in Manrope, and the club's purple
   pin on the point, in the middle.
3. **Picture:** Gotenberg's Chromium takes a picture of the SVG
   (`/forms/chromium/screenshot/html`, 1280 × 480, JPEG quality 82, about
   80 KB, half a second after the page loads; no JavaScript needed). The page embeds its font: **Gotenberg reaches nothing outside
   the page**. No WebGL either: a renderer such as MapLibre needs it, and
   Gotenberg's Chromium has no GPU and no software fallback switch.
4. **Storage:** the JPEG goes through the image pipeline of `event_map`
   (re-encoded, no sizes) into the public bucket, and the Event's
   `map_media_id` links it in the role `event_map` (Media attachment
   written by the statement's trigger).

Credit: "© OpenStreetMap contributors" (ODbL), answered as `mapAttribution`
with `mapAttributionUrl` (`https://www.openstreetmap.org/copyright`, which the
site links it to) and printed under the map as "© OpenStreetMap contributors ·
openstreetmap.org/copyright" (OSMF's attribution guidelines for print).

## When

- A **worker** (`media.MapWorker`) walks the current Events (drafts too)
  with a point and no map, once a minute: each Event is claimed in a short
  transaction before its map is drawn, so the replicas of a rolling deploy
  never draw one twice.
- A point **just set waits a minute** (`map_retry_at`): a pin moved a few
  times in the panel is drawn once.
- A **new point** clears the map (`PATCH` with another `locationLat` /
  `locationLng`); its Media loses its attachment, expires like any detached
  Media, and the worker draws the new point. A body without the point
  keeps the map. Clearing the point clears the map.
- A **copy** with the same point shares the source's map (the copy writes
  its own Media attachment); a later move gives it its own.
- **Failure of one map:** tried again after 10 minutes, each failure in a
  row twice as late (at most a day), given up after 8 (`map_state =
  'failed'`): the Event shows its place without a map until its point
  changes. A map Gotenberg refuses (an error status, an answer that is not a
  JPEG, a request that times out) or a tile that does not parse is such a
  failure: its try is counted and **the pass goes on with the next Event**,
  so one point Gotenberg always refuses never holds up the others. A point
  outside the archive is given up at once.
- **Nothing can be drawn:** while the tiles cannot be read, Gotenberg cannot
  be reached at all (no address, nothing listening), or the archive is of
  another basemap (its metadata names another than the Protomaps basemap
  4.x), no try is counted: the pass stops and the worker waits (30 s,
  doubling up to 15 minutes), saying so once in the log.
- **Metrics** (`/v1/metrics`): `skylab_event_maps_total{outcome}`, `made`,
  `retried`, `given_up`, `no_map`, `panicked`, `discarded`, `source_down`
  (passes stopped).
- The log names Events by id, never a point or the tiles' address; the
  start line names the tiles' host only.

## Settings

| Name | Default | |
|---|---|---|
| `MAP_TILES_URL` | empty: no map | the PMTiles archive, `http(s)`, no credentials. Not a secret. Needs Gotenberg (`GOTENBERG_URL`) and R2 too; without any of them Events show their place without a map. |

Migration `20261122160000_event_map_image`: `events.map_media_id` (FK to
`media`, `ON DELETE SET NULL`), `map_state`, `map_attempts`, `map_retry_at`,
`map_claim_id`, `map_claimed_until`, their check, two indexes, the three
statement triggers of the role `event_map`, and the role tables
(`media_role_purposes`, `media_roles_without_legacy`). Down: the columns and
the maps' Media attachments go (the maps can be drawn again).

## The archive

- **Where:** an object in the public R2 bucket, e.g.
  `maps/turkiye-20261122.pmtiles`, at its custom domain. R2 answers HTTP
  Range (PMTiles needs it). The panel's map reads the same file from the
  browser, so the bucket's CORS allows `GET` and `HEAD` with the `Range`
  header from the panel's origin and exposes `ETag`, `Content-Range` and
  `Content-Length`. A static server on the internal network works for core
  too, but not for the panel.
- **Extent:** Türkiye's bounding box (25.66,35.81,44.83,42.11) to zoom 15,
  measured from the Protomaps build of 2026-11-22: **about 1.6 GB**
  (zoom 15 alone 760 MB). Istanbul alone (27.95,40.80,29.95,41.60):
  **about 75 MB**. A point outside the extract gets no map.
- **Making it** (the `pmtiles` CLI of protomaps/go-pmtiles):
  `pmtiles extract https://build.protomaps.com/<YYYYMMDD>.pmtiles turkiye-<YYYYMMDD>.pmtiles --bbox=25.66,35.81,44.83,42.11 --maxzoom=15`
  (only the ranges in the box are downloaded). `--dry-run` prints the size
  first. The daily builds are listed at
  `https://build-metadata.protomaps.dev/builds.json`.
- **Refreshing:** once or twice a year: extract a new **dated** file,
  upload it next to the old one, check it, point `MAP_TILES_URL` at it and
  redeploy; remove the old file a week later. **Never overwrite a file in
  place:** core keeps an archive's header and root directory for as long as
  it runs, and a changed file under the same address would be read with
  the old directory. Keep the file as Protomaps writes it: its metadata's
  `attribution` and the data are what ODbL asks to pass on. Maps already
  drawn stay as they are; a moved point is drawn from the new file. (A
  wizard under `ops/wizards/` in the hub does these steps.)
- **Checking a file:** `core-backend map-check -url <address> [-point
  lat,lng]`, run in the core container, draws the map at YTÜ Davutpaşa (or
  the point) from the file and prints a JSON report: zooms, bounds, the
  basemap's name, version and attribution, the features of each layer
  drawn, the names written, and `problems`. It exits 0 when the map can be
  drawn from the file, 1 with problems (another basemap or major version,
  no attribution, no zoom 15, no land, roads or buildings at the point, no
  map of it), 2 when the file cannot be read.

## The panel's map

The panel's point picker shows the same archive: MapLibre GL JS with the
`pmtiles` protocol, the Protomaps basemap style (`@protomaps/basemaps`, the
grey "grayscale" or "light" flavor), and its glyphs and sprites
(`protomaps/basemaps-assets`) served from the panel's own files, so the
browser asks no third party. Its credit is the same line. Clicking or
dragging the pin sends `locationLat` and `locationLng` with `PATCH
/v1/events/{id}`; the panel then shows `mapImageUrl` when the worker has
drawn it.
