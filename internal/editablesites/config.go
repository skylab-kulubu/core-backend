package editablesites

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Env configures the sites: a JSON list of {"clientId", "name", "url"}, in
// the order the admin panel shows them. Unset or empty means no site, and
// every editableSites answer is empty. Core refuses to start with a value it
// cannot read.
const Env = "CMS_SITES"

// ErrSitesInvalid is an Env value core cannot read.
var ErrSitesInvalid = errors.New(Env + ": invalid")

// Site is one Site client with a CMS tenant (CONTEXT "Site client"): its
// Keycloak client id, the name the panel shows and the site's address. The
// site's editor sign-in is under URL (`<url>/api/signin`).
type Site struct {
	ClientID string `json:"clientId"`
	Name     string `json:"name"`
	URL      string `json:"url"`
}

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// SitesFromEnv reads Env.
func SitesFromEnv(getenv func(string) string) ([]Site, error) {
	value := strings.TrimSpace(getenv(Env))
	if value == "" {
		return []Site{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(value)))
	decoder.DisallowUnknownFields()
	var sites []Site
	if err := decoder.Decode(&sites); err != nil {
		return nil, fmt.Errorf("%w: not a JSON list of {clientId, name, url}: %v", ErrSitesInvalid, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("%w: more than one JSON value", ErrSitesInvalid)
	}
	seen := map[string]bool{}
	for i := range sites {
		site := &sites[i]
		if !clientIDPattern.MatchString(site.ClientID) {
			return nil, fmt.Errorf("%w: site %d: clientId %q is not a Keycloak client id", ErrSitesInvalid, i+1, site.ClientID)
		}
		if seen[site.ClientID] {
			return nil, fmt.Errorf("%w: %s named twice", ErrSitesInvalid, site.ClientID)
		}
		seen[site.ClientID] = true
		site.Name = strings.TrimSpace(site.Name)
		if site.Name == "" {
			return nil, fmt.Errorf("%w: %s has no name", ErrSitesInvalid, site.ClientID)
		}
		address, err := siteURL(site.URL)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrSitesInvalid, site.ClientID, err)
		}
		site.URL = address
	}
	return sites, nil
}

// siteURL is an absolute http(s) address without credentials, query or
// fragment, and without a trailing slash, so that the panel can append a
// path to it.
func siteURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("url is not an address")
	}
	if scheme := strings.ToLower(u.Scheme); (scheme != "http" && scheme != "https") || u.Host == "" || u.Opaque != "" {
		return "", errors.New("url is not an absolute http(s) address")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("url carries credentials, a query or a fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
