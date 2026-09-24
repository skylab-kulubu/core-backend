package shorturl

import (
	"strings"
	"testing"
)

func TestUTMFillIntoAddsOnlyMissingTags(t *testing.T) {
	t.Parallel()
	utm := UTM{Source: "linkedin", Medium: "social", Campaign: "tanitim"}
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"no query", "https://skylab.com/kayit", "https://skylab.com/kayit?utm_campaign=tanitim&utm_medium=social&utm_source=linkedin"},
		{"keeps target query as sent", "https://skylab.com/?b=2&a=x%20y", "https://skylab.com/?b=2&a=x%20y&utm_campaign=tanitim&utm_medium=social&utm_source=linkedin"},
		{"target tag wins", "https://skylab.com/?utm_source=poster", "https://skylab.com/?utm_source=poster&utm_campaign=tanitim&utm_medium=social"},
		{"all tags present", "https://skylab.com/?utm_source=a&utm_medium=b&utm_campaign=c", "https://skylab.com/?utm_source=a&utm_medium=b&utm_campaign=c"},
		{"keeps fragment", "https://skylab.com/page#form", "https://skylab.com/page?utm_campaign=tanitim&utm_medium=social&utm_source=linkedin#form"},
	}
	for _, tc := range cases {
		if got := utm.FillInto(tc.target); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
	if got := (UTM{}).FillInto("https://skylab.com/?x=1"); got != "https://skylab.com/?x=1" {
		t.Fatalf("empty utm changed target: %s", got)
	}
}

func TestUTMFillIntoEscapesVisitorValues(t *testing.T) {
	t.Parallel()
	got := UTM{Source: "a&redirect=https://evil.example"}.FillInto("https://skylab.com/")
	if got != "https://skylab.com/?utm_source=a%26redirect%3Dhttps%3A%2F%2Fevil.example" {
		t.Fatalf("got %s", got)
	}
}

func TestUTMFromQueryReadsStandardKeysAndCleansValues(t *testing.T) {
	t.Parallel()
	query := map[string]string{
		"utm_source":   "  linkedin ",
		"utm_medium":   "so\x00ci\nal",
		"utm_campaign": "bad\xffutf8",
		"utm_content":  strings.Repeat("ğ", maxUTMValueRunes+10),
		"utm_id":       "ignored",
		"redirect":     "ignored",
	}
	got := UTMFromQuery(func(key string) string { return query[key] })
	if got.Source != "linkedin" || got.Medium != "social" || got.Campaign != "badutf8" || got.Term != "" {
		t.Fatalf("utm %+v", got)
	}
	if n := len([]rune(got.Content)); n != maxUTMValueRunes {
		t.Fatalf("content runes = %d", n)
	}
	if !(UTMFromQuery(func(string) string { return "" })).IsZero() {
		t.Fatal("empty query produced tags")
	}
}
