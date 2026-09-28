package shorturl

import (
	"net/url"
	"strings"
)

// MediumReferral marks a source core inferred from the hop itself rather than
// from a tag on the link.
const MediumReferral = "referral"

// InferSource names the channel of an untagged hop from the browser that made
// it. Instagram and LinkedIn open links in in-app browsers that name
// themselves, and YouTube sends links through its own redirect. WhatsApp, mail
// apps and camera scans open the plain browser without a trace, so they stay
// untagged.
func InferSource(userAgent, referer string) string {
	switch {
	case strings.Contains(userAgent, "Instagram"):
		return "instagram"
	case strings.Contains(userAgent, "LinkedInApp"):
		return "linkedin"
	}
	u, err := url.Parse(strings.TrimSpace(referer))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	within := func(domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }
	switch {
	case within("instagram.com"):
		return "instagram"
	case within("linkedin.com") || host == "lnkd.in":
		return "linkedin"
	case within("youtube.com") || host == "youtu.be":
		return "youtube"
	}
	return ""
}

// WithInferredSource fills in the source of an untagged hop when the hop
// gives it away; a tag the link already carries always wins.
func (u UTM) WithInferredSource(userAgent, referer string) UTM {
	if u.Source != "" {
		return u
	}
	source := InferSource(userAgent, referer)
	if source == "" {
		return u
	}
	u.Source = source
	if u.Medium == "" {
		u.Medium = MediumReferral
	}
	return u
}
