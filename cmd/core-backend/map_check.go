package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/mapimage"
)

// mapCheckCommandName checks a PMTiles archive before core reads it
// (docs/event-map.md, "Refreshing"): the tiles' refresh runs it on the new
// file, from the core container, before it points MAP_TILES_URL at it.
const mapCheckCommandName = "map-check"

// runMapCheck draws the map at a known point (YTÜ Davutpaşa unless -point
// says otherwise) from the archive at -url, prints the report as JSON on
// stdout, and exits 0 when the map can be drawn from it, 1 when it has
// problems, 2 on a usage or reading error.
func runMapCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(mapCheckCommandName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("url", "", "the PMTiles archive's http(s) address")
	at := fs.String("point", fmt.Sprintf("%g,%g", mapimage.CheckPoint[0], mapimage.CheckPoint[1]), "latitude,longitude")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: core-backend %s -url https://…/maps/istanbul-YYYYMMDD.pmtiles [-point lat,lng]\n", mapCheckCommandName)
		return 2
	}
	raw, err := mapimage.TilesURLFromEnv(func(string) string { return *address })
	if err != nil || raw == "" {
		fmt.Fprintf(stderr, "core-backend %s: -url wants an http(s) address of a PMTiles archive\n", mapCheckCommandName)
		return 2
	}
	latText, lngText, ok := strings.Cut(*at, ",")
	lat, errLat := strconv.ParseFloat(strings.TrimSpace(latText), 64)
	lng, errLng := strconv.ParseFloat(strings.TrimSpace(lngText), 64)
	if !ok || errLat != nil || errLng != nil {
		fmt.Fprintf(stderr, "core-backend %s: -point wants latitude,longitude\n", mapCheckCommandName)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := mapimage.Check(ctx, mapimage.HTTPSource{URL: raw, Client: &http.Client{Timeout: time.Minute}}, lat, lng)
	if err != nil {
		fmt.Fprintf(stderr, "core-backend %s: %v\n", mapCheckCommandName, err)
		return 2
	}
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	if err := out.Encode(report); err != nil {
		return 2
	}
	if len(report.Problems) > 0 {
		return 1
	}
	return 0
}
