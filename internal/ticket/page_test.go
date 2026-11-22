package ticket_test

import (
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func TestPageConfigFromEnv(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env   map[string]string
		ok    bool
		url   string
		grace time.Duration
	}{
		"unset":         {map[string]string{}, true, "", 7 * 24 * time.Hour},
		"site":          {map[string]string{ticket.PageURLEnv: "https://yildizskylab.com/bilet/{key}"}, true, "https://yildizskylab.com/bilet/{key}", 7 * 24 * time.Hour},
		"grace":         {map[string]string{ticket.PageGraceEnv: "72h"}, true, "", 72 * time.Hour},
		"http":          {map[string]string{ticket.PageURLEnv: "http://yildizskylab.com/bilet/{key}"}, false, "", 0},
		"no key":        {map[string]string{ticket.PageURLEnv: "https://yildizskylab.com/bilet/"}, false, "", 0},
		"key twice":     {map[string]string{ticket.PageURLEnv: "https://yildizskylab.com/{key}/{key}"}, false, "", 0},
		"query":         {map[string]string{ticket.PageURLEnv: "https://yildizskylab.com/bilet?k={key}"}, false, "", 0},
		"bad grace":     {map[string]string{ticket.PageGraceEnv: "bir hafta"}, false, "", 0},
		"grace too big": {map[string]string{ticket.PageGraceEnv: "2400h"}, false, "", 0},
	} {
		cfg, err := ticket.PageConfigFromEnv(func(k string) string { return tc.env[k] })
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if tc.ok && (cfg.URL != tc.url || cfg.Grace != tc.grace) {
			t.Errorf("%s: %+v", name, cfg)
		}
	}
	cfg := ticket.PageConfig{URL: "https://yildizskylab.com/bilet/{key}", Grace: 7 * 24 * time.Hour}
	if got := cfg.Link("ABCDEFGHIJKLMNOPQRSTUVWXYZ"); got != "https://yildizskylab.com/bilet/ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Errorf("link %q", got)
	}
	if cfg.Link("") != "" || (ticket.PageConfig{}).Link("ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		t.Error("a link without a key or an address")
	}
	end := time.Date(2026, 12, 12, 15, 0, 0, 0, time.UTC)
	ev := event.Event{EndDate: &end}
	if cfg.Closed(ev, end.Add(6*24*time.Hour)) || !cfg.Closed(ev, end.Add(7*24*time.Hour)) || cfg.Closed(event.Event{}, end.Add(1000*time.Hour)) {
		t.Error("Closed")
	}
}
