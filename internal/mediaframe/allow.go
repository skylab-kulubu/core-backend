package mediaframe

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ErrURLNotAllowed is a video address the frame service does not read: not
// https, not on an allowed host (named by its name, never an IP address), on
// another port than 443, or with credentials in it.
var ErrURLNotAllowed = errors.New("mediaframe: the video address is not an https address on an allowed host")

// maxURLBytes bounds a video address: a presigned GET is a few hundred bytes.
const maxURLBytes = 4096

// hostName is a DNS name of at least two labels whose last label holds a
// letter: never an IP address in any of its spellings.
var hostName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseAllowedHosts reads the allowlist (MEDIA_FRAME_ALLOWED_HOSTS): host
// names, or https addresses of the storage endpoint such as core's
// R2_ENDPOINT, separated by commas. An address counts only for its host.
// Anything that would widen the list is refused: a pattern, an IP address,
// a port, a path, credentials, plain http, or nothing at all.
func ParseAllowedHosts(raw string) ([]string, error) {
	var hosts []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host := entry
		if strings.Contains(entry, "://") {
			u, err := url.Parse(entry)
			if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") ||
				u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("mediaframe: %q is not a host name or an https address with no path", entry)
			}
			host = u.Hostname()
		}
		host = strings.ToLower(host)
		if !hostName.MatchString(host) || net.ParseIP(host) != nil {
			return nil, fmt.Errorf("mediaframe: %q is not a host name (no IP address, pattern or port)", entry)
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return nil, errors.New("mediaframe: no allowed host")
	}
	return hosts, nil
}

// checkURL is the video address when the service may read it: https, on
// one of the allowed hosts, on port 443, without credentials.
func checkURL(raw string, allowed []string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return nil, ErrURLNotAllowed
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" {
		return nil, ErrURLNotAllowed
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, ErrURLNotAllowed
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil || !hostName.MatchString(host) {
		return nil, ErrURLNotAllowed
	}
	for _, allowedHost := range allowed {
		if host == allowedHost {
			return u, nil
		}
	}
	return nil, ErrURLNotAllowed
}
