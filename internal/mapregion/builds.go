package mapregion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The Protomaps daily builds: a list of the builds kept (the newest few)
// and the builds themselves, public, without a key.
const (
	DefaultBuildsURL = "https://build-metadata.protomaps.dev/builds.json"
	DefaultBuildBase = "https://build.protomaps.com/"
)

// ErrNoBuild is a builds list core cannot read or that names no build it
// can extract from.
var ErrNoBuild = errors.New("mapregion: no Protomaps build to extract from")

// Build is one Protomaps build.
type Build struct {
	// Date is the build's day, YYYYMMDD; URL its archive; Version the
	// basemap's.
	Date, URL, Version string
}

// Builds reads the latest Protomaps build.
type Builds struct {
	ListURL, Base string
	Client        *http.Client
}

var buildKey = regexp.MustCompile(`^(\d{8})\.pmtiles$`)

// Latest is the newest build of the basemap's major version 4 (the one Event
// maps are drawn for).
func (b Builds) Latest(ctx context.Context) (Build, error) {
	listURL, base := b.ListURL, b.Base
	if listURL == "" {
		listURL = DefaultBuildsURL
	}
	if base == "" {
		base = DefaultBuildBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return Build{}, err
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Build{}, fmt.Errorf("%w: the list does not answer", ErrNoBuild)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Build{}, fmt.Errorf("%w: the list answers HTTP %d", ErrNoBuild, resp.StatusCode)
	}
	var builds []struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&builds); err != nil {
		return Build{}, fmt.Errorf("%w: the list is not JSON", ErrNoBuild)
	}
	sort.SliceStable(builds, func(i, j int) bool { return builds[i].Key > builds[j].Key })
	for _, build := range builds {
		m := buildKey.FindStringSubmatch(build.Key)
		if m == nil || !strings.HasPrefix(build.Version, "4.") {
			continue
		}
		address, err := url.JoinPath(base, build.Key)
		if err != nil {
			return Build{}, err
		}
		return Build{Date: m[1], URL: address, Version: build.Version}, nil
	}
	return Build{}, fmt.Errorf("%w: no build of version 4", ErrNoBuild)
}
