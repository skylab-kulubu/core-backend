package shorturl

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

const (
	iphoneUA  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148"
	androidUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/120.0 Mobile Safari/537.36"
	desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36"
)

func statsFixture(t *testing.T) (Service, *MemoryStore, authz.Principal, URL) {
	t.Helper()
	store := NewMemoryStore()
	svc := NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	owner := authz.Principal{ID: "11111111-1111-1111-1111-111111111111", Roles: []string{"url:access"}}
	created, err := svc.Create(context.Background(), owner, "https://skylab.com", "club")
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, owner, created
}

func record(t *testing.T, store *MemoryStore, id uuid.UUID, h Hit) {
	t.Helper()
	if _, err := store.RecordHit(context.Background(), id, h); err != nil {
		t.Fatal(err)
	}
}

func TestLinkStatsCountsByDayChannelSiteAndDevice(t *testing.T) {
	t.Parallel()
	svc, store, owner, link := statsFixture(t)
	loc, _ := time.LoadLocation(LinkStatsTimeZone)
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)

	record(t, store, link.ID, Hit{CreatedAt: today, IP: "203.0.113.9", UserAgent: iphoneUA, Referer: "https://www.instagram.com/p/x", UTM: UTM{Source: "ig"}})
	record(t, store, link.ID, Hit{CreatedAt: today, IP: "203.0.113.10", UserAgent: androidUA, UTM: UTM{Source: "qr", Medium: "qr"}})
	record(t, store, link.ID, Hit{CreatedAt: yesterday, IP: "203.0.113.11", UserAgent: desktopUA, Referer: "https://skylab.com/etkinlik", UTM: UTM{Source: "ogrenci-12345"}})
	record(t, store, link.ID, Hit{CreatedAt: yesterday, UserAgent: "WhatsApp/2.23.20 A"})
	// Older than the window: left out.
	record(t, store, link.ID, Hit{CreatedAt: today.AddDate(0, 0, -10), UserAgent: desktopUA})

	stats, err := svc.LinkStats(context.Background(), owner, link.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Days != 7 || len(stats.Daily) != 7 || stats.Total != 4 || stats.Scans != 1 {
		t.Fatalf("totals %+v", stats)
	}
	if got := stats.Daily[6]; got.Day != today.Format(time.DateOnly) || got.Count != 2 {
		t.Fatalf("today %+v", got)
	}
	if got := stats.Daily[5]; got.Day != yesterday.Format(time.DateOnly) || got.Count != 2 {
		t.Fatalf("yesterday %+v", got)
	}
	if stats.Daily[0].Count != 0 || stats.Daily[0].Day != today.AddDate(0, 0, -6).Format(time.DateOnly) {
		t.Fatalf("first day %+v", stats.Daily[0])
	}
	sources := map[string]SourceCount{}
	for _, c := range stats.Sources {
		sources[c.Source] = c
	}
	if sources["instagram"].Count != 1 || sources["qr"].Scans != 1 || sources[OtherChannel].Count != 1 || sources[""].Count != 1 {
		t.Fatalf("sources %+v", stats.Sources)
	}
	referrers := map[string]int{}
	for _, c := range stats.Referrers {
		referrers[c.Name] = c.Count
	}
	if referrers["instagram.com"] != 1 || referrers["skylab.com"] != 1 || referrers[""] != 2 {
		t.Fatalf("referrers %+v", stats.Referrers)
	}
	devices := map[string]int{}
	for _, c := range stats.Devices {
		devices[c.Name] = c.Count
	}
	if devices[DeviceMobile] != 2 || devices[DeviceDesktop] != 1 || devices[DeviceBot] != 1 {
		t.Fatalf("devices %+v", stats.Devices)
	}
}

func TestLinkStatsCarriesNothingPersonal(t *testing.T) {
	t.Parallel()
	svc, store, owner, link := statsFixture(t)
	userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	record(t, store, link.ID, Hit{
		IP:        "203.0.113.9",
		UserAgent: iphoneUA,
		Referer:   "https://forum.example/u/ayse?token=abc",
		UserID:    &userID,
		UTM:       UTM{Source: "ayse@example.com", Campaign: "kampanya-ozel", Term: "terim", Content: "icerik"},
	})
	stats, err := svc.LinkStats(context.Background(), owner, link.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Days != LinkStatsDays || stats.Total != 1 {
		t.Fatalf("stats %+v", stats)
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"203.0.113.9", "iPhone", "/u/ayse", "token", userID.String(), "ayse@example.com", "kampanya-ozel", "terim", "icerik"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("summary carries %q: %s", leak, raw)
		}
	}
}

func TestLinkStatsReaders(t *testing.T) {
	t.Parallel()
	svc, _, _, link := statsFixture(t)
	stranger := authz.Principal{ID: "33333333-3333-3333-3333-333333333333", Roles: []string{"url:access"}}
	if _, err := svc.LinkStats(context.Background(), stranger, link.ID, 30); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger: %v", err)
	}
	moderator := authz.Principal{ID: "44444444-4444-4444-4444-444444444444", Roles: []string{"url:moderator"}}
	if _, err := svc.LinkStats(context.Background(), moderator, link.ID, 30); err != nil {
		t.Fatalf("moderator: %v", err)
	}
	if _, err := svc.LinkStats(context.Background(), moderator, link.ID, LinkStatsMaxDays+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too long: %v", err)
	}
	if _, err := svc.LinkStats(context.Background(), moderator, uuid.New(), 30); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown link: %v", err)
	}
}

func TestLinkStatsNamesTenSitesAndCountsTheRestTogether(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"other": 1}
	for i := 0; i < 12; i++ {
		counts[string(rune('a'+i))+".example"] = 20 - i
	}
	top := topNamed(counts, linkStatsTop)
	if len(top) != linkStatsTop+1 {
		t.Fatalf("rows %d: %+v", len(top), top)
	}
	total := 0
	for _, c := range top {
		total += c.Count
	}
	if total != 1+20+19+18+17+16+15+14+13+12+11+10+9 {
		t.Fatalf("total %d: %+v", total, top)
	}
}

func TestDeviceOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":        DeviceUnknown,
		iphoneUA:  DeviceMobile,
		androidUA: DeviceMobile,
		desktopUA: DeviceDesktop,
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X)":                       DeviceTablet,
		"Mozilla/5.0 (Linux; Android 13; SM-X200) AppleWebKit/537.36 Safari":  DeviceTablet,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15":   DeviceDesktop,
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot)": DeviceBot,
		"TelegramBot (like TwitterBot)":                                       DeviceBot,
		"facebookexternalhit/1.1":                                             DeviceBot,
		"Instagram 300.0 (iPhone14,2; iOS 17_0; tr_TR) AppleWebKit Mobile":    DeviceMobile,
		"something odd": DeviceUnknown,
	}
	for ua, want := range cases {
		if got := DeviceOf(ua); got != want {
			t.Errorf("DeviceOf(%q) = %q, want %q", ua, got, want)
		}
	}
}
