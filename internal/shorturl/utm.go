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

// channelSources maps the two-letter suffix of a shared link
// (skyl.app/alias/ig) to the utm_source it stands for, so the short form and
// the ?utm_source= form of the same channel land in the same bucket. The same
// code is printed in the corner of that channel's QR code.
var channelSources = map[string]string{
	"ig": "instagram",
	"wa": "whatsapp",
	"in": "linkedin",
	"yt": "youtube",
	"ma": "email",
}

// ChannelUTM is the tag behind a channel suffix. An unknown suffix carries no
// tag, so a mistyped printed link still reaches its target.
func ChannelUTM(code string) UTM {
	return UTM{Source: channelSources[strings.ToLower(strings.TrimSpace(code))]}
}

// ChannelCode is the suffix of a channel source, or "" for a source without
// one (qr, or a name someone typed).
func ChannelCode(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	for code, s := range channelSources {
		if s == source {
			return code
		}
	}
	return ""
}

// OtherChannel is what a click's utm_source or utm_medium becomes, once the
// click is a year old, when it is not a known channel.
const OtherChannel = "other"

// KnownSources maps each utm_source spelling the statistics know to its
// channel: the channels core writes (channelSources, InferSource, qr) and
// Forms offers (its share channels and SOURCE_LABELS), with the spellings
// Forms' AttributionNormalizer reads as the same channel. The source and
// medium are written as they came: a form's statistics show a channel the
// organizer named under Forms' "Diğer" for as long as they look back
// (HitListWindow). A year on, the retention sweep (url_hits_scrub v3, in
// apply) keeps a known channel, lower-cased and in its usual spelling, and
// makes anything else OtherChannel: whatever a visitor typed into the
// address could be an e-mail or a student number. The keys and values are
// SQL literals in that rule and in its index: plain lower-case words, and
// every channel its own key.
var KnownSources = map[string]string{
	"instagram": "instagram", "ig": "instagram", "insta": "instagram",
	"whatsapp": "whatsapp", "wa": "whatsapp", "wp": "whatsapp",
	"linkedin": "linkedin", "in": "linkedin", "li": "linkedin",
	"youtube": "youtube", "yt": "youtube",
	"email": "email", "ma": "email", "mail": "email", "e-mail": "email", "e-posta": "email", "eposta": "email",
	"x": "x", "twitter": "x",
	"website": "website", "web": "website", "site": "website",
	"qr":         "qr",
	OtherChannel: OtherChannel,
}

// KnownMediums are the utm_medium values the statistics know: qr (a printed
// code) and referral (MediumReferral) from core, and the mediums Forms
// derives from a source. The sweep treats them as KnownSources.
var KnownMediums = map[string]string{
	"qr": "qr", MediumReferral: MediumReferral, "social": "social", "messaging": "messaging",
	"email": "email", "print": "print", OtherChannel: OtherChannel,
}

// KeptSource is what a year-old click keeps of its utm_source; KeptMedium of
// its utm_medium. Untagged stays untagged.
func KeptSource(raw string) string { return keptChannel(KnownSources, raw) }

func KeptMedium(raw string) string { return keptChannel(KnownMediums, raw) }

func keptChannel(known map[string]string, raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return ""
	}
	if channel, ok := known[v]; ok {
		return channel
	}
	return OtherChannel
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
