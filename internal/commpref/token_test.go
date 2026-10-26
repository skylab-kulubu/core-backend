package commpref

import (
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
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
	if string(derive(testKey(), linkPurpose)) == string(testKey()) {
		t.Fatal("the link key is the raw key")
	}
}

// D3: the Account center link names the row to show first, keeps a query
// the setting already has, and carries no token.
func TestAccountCenterLink(t *testing.T) {
	config := TestConfig(testKey(), "https://api.example.test")
	if got := config.AccountCenterLink(ChannelEmail, Team("g-weblab", KindAnnouncement)); got != "https://my.yildizskylab.com?kanal=email&kategori=team%3Ag-weblab%3Aduyuru" {
		t.Fatalf("link %q", got)
	}
	config.AccountCenterURL = "https://my.example.test/iletisim?sekme=1"
	if got := config.AccountCenterLink(ChannelEmail, Club(KindNews)); got != "https://my.example.test/iletisim?kanal=email&kategori=club%3Ahaber&sekme=1" {
		t.Fatalf("link %q", got)
	}
	if got := config.AccountCenterLink(Channel("push"), Club(KindNews)); got != config.AccountCenterURL {
		t.Fatalf("unknown channel %q", got)
	}
}

// goldenFormatOne is a format 1 token that main signed before the key ring
// (abonelik-cikisi 13) with testKey(): every link already mailed looks like
// it, and must keep reading.
const goldenFormatOne = "AXUBbxwtPktaTG2Of5ChssPU5QIBBCQyYjBmOGE0ZS0wZjRjLTRhNDMtOWIwYi0xZDFlMmYzYTRiNWNqYVfXkwjP-gPq6nRmOXl3"

var goldenPerson = uuid.MustParse("6f1c2d3e-4b5a-4c6d-8e7f-90a1b2c3d4e5")

func goldenCategories() []Category {
	return []Category{Club(KindAnnouncement), Team("2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c", KindNews)}
}

func ringKey(seed byte) []byte {
	key := testKey()
	for i := range key {
		key[i] ^= seed
	}
	return key
}

func ringEnv(values map[string]string) func(string) string {
	full := map[string]string{"PUBLIC_API_ORIGIN": "https://api.example.test"}
	for name, value := range values {
		full[name] = value
	}
	return func(name string) string { return full[name] }
}

func encodedKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

func ringConfig(t *testing.T, values map[string]string) Config {
	t.Helper()
	config, err := ConfigFromEnv(ringEnv(values))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func assertGolden(t *testing.T, config Config, token string) {
	t.Helper()
	person, ch, cats, err := config.readToken(token)
	if err != nil || person != goldenPerson || ch != ChannelEmail {
		t.Fatalf("read: %v %v %v", person, ch, err)
	}
	want := goldenCategories()
	if len(cats) != len(want) || cats[0] != want[0] || cats[1] != want[1] {
		t.Fatalf("categories = %v, want %v", cats, want)
	}
}

// A link mailed before the ring reads as it did, under the version 1 key
// alone and in a ring whose current version is newer; with the current
// version unset or 1 core still signs exactly that token, so an image
// rolled back still reads every link the new one mailed.
func TestFormatOneLinksOfMainStillRead(t *testing.T) {
	assertGolden(t, TestConfig(testKey(), "https://api.example.test"), goldenFormatOne)
	for name, values := range map[string]map[string]string{
		"key alone":         {KeyEnv: encodedKey(testKey())},
		"current 1":         {KeyEnv: encodedKey(testKey()), CurrentKeyEnv: "1", KeyVersionEnvPrefix + "2": encodedKey(ringKey(2))},
		"current unset":     {KeyEnv: encodedKey(testKey()), KeyVersionEnvPrefix + "2": encodedKey(ringKey(2))},
		"a newer current 2": {KeyEnv: encodedKey(testKey()), CurrentKeyEnv: "2", KeyVersionEnvPrefix + "2": encodedKey(ringKey(2))},
	} {
		config := ringConfig(t, values)
		assertGolden(t, config, goldenFormatOne)
		if values[CurrentKeyEnv] == "2" {
			continue
		}
		token, err := config.sign(goldenPerson, ChannelEmail, goldenCategories())
		if err != nil || token != goldenFormatOne {
			t.Fatalf("%s: signs %q %v, want the format 1 token", name, token, err)
		}
	}
}

// With a current version of 2 or more a new link is format 2: the version
// byte 2 and the key id, then what format 1 carries. Every version in the
// ring reads its own links; a version not in the ring, or format 2 naming
// version 1, does not read.
func TestRingSignsWithTheCurrentVersionAndReadsEveryVersion(t *testing.T) {
	old := TestConfig(testKey(), "https://api.example.test")
	oldToken, err := old.sign(goldenPerson, ChannelEmail, goldenCategories())
	if err != nil {
		t.Fatal(err)
	}
	ring := ringConfig(t, map[string]string{
		KeyEnv: encodedKey(testKey()), KeyVersionEnvPrefix + "2": encodedKey(ringKey(2)), KeyVersionEnvPrefix + "3": encodedKey(ringKey(3)), CurrentKeyEnv: "3",
	})
	token, err := ring.sign(goldenPerson, ChannelEmail, goldenCategories())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || raw[0] != 2 || raw[1] != 3 || raw[2] != 'u' {
		t.Fatalf("format 2 token starts %v %v", raw[:3], err)
	}
	assertGolden(t, ring, token)
	assertGolden(t, ring, oldToken)
	if _, _, _, err := old.readToken(token); !errors.Is(err, ErrLink) {
		t.Fatalf("version 1 alone reads a version 3 token: %v", err)
	}
	two := ringConfig(t, map[string]string{KeyEnv: encodedKey(testKey()), KeyVersionEnvPrefix + "2": encodedKey(ringKey(2)), CurrentKeyEnv: "2"})
	twoToken, err := two.sign(goldenPerson, ChannelEmail, goldenCategories())
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, ring, twoToken)
	// The key id is signed: another id over the same bytes does not read.
	for _, id := range []byte{0, 1, 2, 4, 255} {
		changed := append([]byte(nil), raw...)
		changed[1] = id
		if _, _, _, err := ring.readToken(base64.RawURLEncoding.EncodeToString(changed)); !errors.Is(err, ErrLink) {
			t.Fatalf("key id %d reads: %v", id, err)
		}
	}
	for i := range raw {
		changed := append([]byte(nil), raw...)
		changed[i] ^= 0x01
		if _, _, _, err := ring.readToken(base64.RawURLEncoding.EncodeToString(changed)); err == nil {
			t.Fatalf("byte %d changed and the format 2 token still reads", i)
		}
	}
	// A format 2 token of version 1, signed with the version 1 key: core
	// never makes one, and a token has one spelling.
	payload := append([]byte{2, 1}, raw[2:len(raw)-macSize]...)
	forged := append(payload, linkMAC(testKey(), payload)...)
	if _, _, _, err := ring.readToken(base64.RawURLEncoding.EncodeToString(forged)); !errors.Is(err, ErrLink) {
		t.Fatalf("format 2 of version 1 reads: %v", err)
	}
	for _, bad := range []string{"Ag", "AgM", base64.RawURLEncoding.EncodeToString([]byte{3, 1, 'u'})} {
		if _, _, _, err := ring.readToken(bad); !errors.Is(err, ErrLink) {
			t.Fatalf("%q reads: %v", bad, err)
		}
	}
}

// A leak: the leaked version's variable goes, and its links stop reading
// at the next start (orkestratör kararı 2026-10-21); the other versions'
// links still read. Version 1 leaked is COMMUNICATION_PREFERENCE_KEY gone.
func TestALeakedVersionRemovedStopsReadingItsLinks(t *testing.T) {
	two := ringConfig(t, map[string]string{KeyEnv: encodedKey(testKey()), KeyVersionEnvPrefix + "2": encodedKey(ringKey(2)), CurrentKeyEnv: "2"})
	twoToken, err := two.sign(goldenPerson, ChannelEmail, goldenCategories())
	if err != nil {
		t.Fatal(err)
	}
	afterLeak := ringConfig(t, map[string]string{KeyVersionEnvPrefix + "2": encodedKey(ringKey(2)), KeyVersionEnvPrefix + "3": encodedKey(ringKey(3)), CurrentKeyEnv: "3"})
	if !afterLeak.Enabled {
		t.Fatal("a ring without version 1 is off")
	}
	if _, _, _, err := afterLeak.readToken(goldenFormatOne); !errors.Is(err, ErrLink) {
		t.Fatalf("a link of the removed version 1 reads: %v", err)
	}
	assertGolden(t, afterLeak, twoToken)
}

func TestKeyRingFromEnv(t *testing.T) {
	one, two := encodedKey(testKey()), encodedKey(ringKey(2))
	config := ringConfig(t, map[string]string{KeyEnv: one, KeyVersionEnvPrefix + "2": two, KeyVersionEnvPrefix + "7": encodedKey(ringKey(7)), CurrentKeyEnv: " 7 "})
	if !config.Enabled || config.CurrentKeyVersion() != 7 || !slices.Equal(config.KeyVersions(), []int{1, 2, 7}) {
		t.Fatalf("ring: %v current %d", config.KeyVersions(), config.CurrentKeyVersion())
	}
	if c := ringConfig(t, map[string]string{KeyEnv: one}); c.CurrentKeyVersion() != 1 || !slices.Equal(c.KeyVersions(), []int{1}) {
		t.Fatalf("key alone: %v current %d", c.KeyVersions(), c.CurrentKeyVersion())
	}
	if c := ringConfig(t, nil); c.Enabled || len(c.KeyVersions()) != 0 {
		t.Fatalf("no key: %+v", c)
	}
	for name, values := range map[string]map[string]string{
		"current names a missing version": {KeyEnv: one, CurrentKeyEnv: "2"},
		"current without any key":         {CurrentKeyEnv: "1"},
		"current 0":                       {KeyEnv: one, CurrentKeyEnv: "0"},
		"current 256":                     {KeyEnv: one, CurrentKeyEnv: "256"},
		"current not a number":            {KeyEnv: one, CurrentKeyEnv: "v2"},
		"a version without a current":     {KeyVersionEnvPrefix + "2": two},
		"version 1 under the _V1 name":    {KeyVersionEnvPrefix + "1": one, CurrentKeyEnv: "1"},
		"two versions, one key":           {KeyEnv: one, KeyVersionEnvPrefix + "2": one, CurrentKeyEnv: "2"},
		"a short version key":             {KeyEnv: one, KeyVersionEnvPrefix + "2": "abc", CurrentKeyEnv: "2"},
		"a ring without an origin":        {KeyEnv: "", KeyVersionEnvPrefix + "2": two, CurrentKeyEnv: "2", "PUBLIC_API_ORIGIN": " "},
	} {
		_, err := ConfigFromEnv(ringEnv(values))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), one) || strings.Contains(err.Error(), two) {
			t.Fatalf("%s: the error shows a key", name)
		}
	}
}
