package commpref

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestUnsubscribeTokenCarriesThePersonAndTheCategories(t *testing.T) {
	config := TestConfig(testKey(), "https://api.example.test/")
	person := uuid.New()
	cats := []Category{Team("2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c", KindAnnouncement), Club(KindNews), Team("g-weblab", KindAnnouncement), Club(KindNews)}
	link, err := config.UnsubscribeURL(person, ChannelEmail, cats)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.example.test" || parsed.Path != UnsubscribePath {
		t.Fatalf("link %q: %v", link, err)
	}
	if strings.Contains(link, person.String()) || strings.Contains(link, "weblab") {
		t.Fatalf("the link shows what it carries in clear: %q", link)
	}
	id, ch, got, err := config.readToken(parsed.Query().Get("t"))
	if err != nil || id != person || ch != ChannelEmail {
		t.Fatalf("read: %v %v", id, err)
	}
	want := []Category{Club(KindNews), Team("2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c", KindAnnouncement), Team("g-weblab", KindAnnouncement)}
	if len(got) != len(want) {
		t.Fatalf("categories = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("categories = %v, want %v", got, want)
		}
	}
}

func TestUnsubscribeTokenRefusesForgeryAndOtherKeys(t *testing.T) {
	config := TestConfig(testKey(), "https://api.example.test")
	person := uuid.New()
	link, err := config.UnsubscribeURL(person, ChannelEmail, []Category{Club(KindAnnouncement)})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	token := parsed.Query().Get("t")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		changed := append([]byte(nil), raw...)
		changed[i] ^= 0x01
		if _, _, _, err := config.readToken(base64.RawURLEncoding.EncodeToString(changed)); err == nil {
			t.Fatalf("byte %d changed and the token still reads", i)
		}
	}
	other := testKey()
	other[0] ^= 0xff
	if _, _, _, err := TestConfig(other, "https://api.example.test").readToken(token); err == nil {
		t.Fatal("a token of another key reads")
	}
	for _, bad := range []string{"", "x", token + "A", token[:len(token)-2], strings.Repeat("A", 4000)} {
		if _, _, _, err := config.readToken(bad); err == nil {
			t.Fatalf("%q reads", bad)
		}
	}
}

func TestUnsubscribeURLRefusesWhatALinkCannotCarry(t *testing.T) {
	config := TestConfig(testKey(), "https://api.example.test")
	if _, err := config.UnsubscribeURL(uuid.New(), ChannelEmail, nil); err == nil {
		t.Fatal("no category")
	}
	if _, err := config.UnsubscribeURL(uuid.New(), ChannelEmail, []Category{Master()}); err == nil {
		t.Fatal("the master switch is no mail's category")
	}
	if _, err := config.UnsubscribeURL(uuid.Nil, ChannelEmail, []Category{Club(KindNews)}); err == nil {
		t.Fatal("no person")
	}
	if _, err := config.UnsubscribeURL(uuid.New(), Channel("push"), []Category{Club(KindNews)}); err == nil {
		t.Fatal("a channel core does not keep")
	}
	many := make([]Category, 0, MaxLinkCategories+1)
	for i := 0; i <= MaxLinkCategories; i++ {
		many = append(many, Team(uuid.NewString(), KindNews))
	}
	if _, err := config.UnsubscribeURL(uuid.New(), ChannelEmail, many); err == nil {
		t.Fatal("too many categories")
	}
	if _, err := (Config{}).UnsubscribeURL(uuid.New(), ChannelEmail, []Category{Club(KindNews)}); err == nil {
		t.Fatal("disabled config signs")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	off, err := ConfigFromEnv(env(nil))
	if err != nil || off.Enabled {
		t.Fatalf("unset: %+v %v", off, err)
	}
	key := base64.RawURLEncoding.EncodeToString(testKey())
	on, err := ConfigFromEnv(env(map[string]string{KeyEnv: key, "PUBLIC_API_ORIGIN": "https://api.example.test/"}))
	if err != nil || !on.Enabled || on.LinkOrigin != "https://api.example.test" || on.AccountCenterURL != DefaultAccountCenterURL {
		t.Fatalf("on: %+v %v", on, err)
	}
	for name, values := range map[string]map[string]string{
		"short key":  {KeyEnv: "abc", "PUBLIC_API_ORIGIN": "https://a.test"},
		"no origin":  {KeyEnv: key},
		"bad origin": {KeyEnv: key, "PUBLIC_API_ORIGIN": "api.example.test"},
		"bad center": {KeyEnv: key, "PUBLIC_API_ORIGIN": "https://a.test", AccountCenterURLEnv: "javascript:alert(1)"},
	} {
		_, err := ConfigFromEnv(env(values))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("%s: the error shows the key", name)
		}
	}
	// The same key as the contact consents' signs other links: the
	// derivation purposes differ.
	if string(on.derive(linkPurpose)) == string(testKey()) {
		t.Fatal("the link key is the raw key")
	}
}
