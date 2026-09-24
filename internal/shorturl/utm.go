package shorturl

import (
	"net/url"
	"strings"
	"unicode"
)

// maxUTMValueRunes bounds each stored tag. The query string is visitor
// controlled, so an unbounded value would let anyone grow url_hits at will.
const maxUTMValueRunes = 200

// UTM holds the campaign tags a short link was opened with.
type UTM struct {
	Source   string `json:"source,omitempty"`
	Medium   string `json:"medium,omitempty"`
	Campaign string `json:"campaign,omitempty"`
	Term     string `json:"term,omitempty"`
	Content  string `json:"content,omitempty"`
}

// UTMFromQuery reads the standard utm_* keys through query, which returns ""
// for a missing key.
func UTMFromQuery(query func(key string) string) UTM {
	return UTM{
		Source:   cleanUTMValue(query("utm_source")),
		Medium:   cleanUTMValue(query("utm_medium")),
		Campaign: cleanUTMValue(query("utm_campaign")),
		Term:     cleanUTMValue(query("utm_term")),
		Content:  cleanUTMValue(query("utm_content")),
	}
}

func (u UTM) IsZero() bool {
	return u == UTM{}
}

func (u UTM) params() [5][2]string {
	return [5][2]string{
		{"utm_source", u.Source},
		{"utm_medium", u.Medium},
		{"utm_campaign", u.Campaign},
		{"utm_term", u.Term},
		{"utm_content", u.Content},
	}
}

// FillInto adds each tag the target does not already carry. Tags the link
// owner baked into the target win: they describe the campaign the link was
// made for, and a visitor editing the short link must not relabel it.
// The target's own query is appended to rather than re-encoded, so a target
// that depends on its exact parameter order or escaping keeps working.
func (u UTM) FillInto(target string) string {
	if u.IsZero() {
		return target
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	existing := parsed.Query()
	extra := url.Values{}
	for _, p := range u.params() {
		if p[1] != "" && !existing.Has(p[0]) {
			extra.Set(p[0], p[1])
		}
	}
	if len(extra) == 0 {
		return target
	}
	if parsed.RawQuery == "" {
		parsed.RawQuery = extra.Encode()
	} else {
		parsed.RawQuery += "&" + extra.Encode()
	}
	return parsed.String()
}

// cleanUTMValue drops control characters and invalid UTF-8 because PostgreSQL
// rejects NUL in text, and a failed insert would silently lose the hit.
func cleanUTMValue(raw string) string {
	v := strings.ToValidUTF8(raw, "")
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
	v = strings.TrimSpace(v)
	if r := []rune(v); len(r) > maxUTMValueRunes {
		v = strings.TrimSpace(string(r[:maxUTMValueRunes]))
	}
	return v
}
