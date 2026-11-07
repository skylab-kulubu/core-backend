package apppref_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
)

func testConfig(t *testing.T) apppref.Config {
	t.Helper()
	config, err := apppref.ParseClients("forms:skyforms, forms:skyforms-local, panel:admin, account:account-center")
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func TestParseClients(t *testing.T) {
	for _, off := range []string{"", "  ", "none"} {
		config, err := apppref.ParseClients(off)
		if err != nil || config.Enabled() {
			t.Fatalf("%q: enabled=%v err=%v", off, config.Enabled(), err)
		}
	}
	config := testConfig(t)
	if !config.Enabled() || config.AppOf("skyforms-local") != "forms" || config.AppOf("frontend-main") != "" || !config.Known("panel") || config.Known("skymail") {
		t.Fatalf("config = %s", config)
	}
	if got := strings.Join(config.Apps(), ","); got != "account,forms,panel" {
		t.Fatalf("apps = %s", got)
	}
	for _, bad := range []string{"forms", "forms:", ":skyforms", "Forms:skyforms", "f:skyforms", "forms:skyforms,panel:skyforms", "forms:sky forms"} {
		if _, err := apppref.ParseClients(bad); err == nil || !strings.Contains(err.Error(), apppref.ClientsEnv) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

func person(client string) apppref.Caller {
	return apppref.Caller{Person: uuid.New(), Client: client}
}

// Each app reaches only its own namespace; Account center reads the summary
// and resets any app; a service account and an unmapped client reach
// nothing; an unknown app is unknown before the client is asked about.
func TestServiceAuthorizesByTheTokensClient(t *testing.T) {
	ctx := context.Background()
	svc := apppref.NewService(testConfig(t), apppref.NewMemoryStore())
	value := json.RawMessage(`{"details":false}`)

	forms := person("skyforms")
	if _, err := svc.Put(ctx, forms, "forms", "forms.list", value, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		err  error
		run  func() error
	}{
		{"forms reads panel", apppref.ErrWrongClient, func() error { _, err := svc.Get(ctx, forms, "panel"); return err }},
		{"forms writes panel", apppref.ErrWrongClient, func() error { _, err := svc.Put(ctx, forms, "panel", "k", value, 1, nil); return err }},
		{"forms resets panel", apppref.ErrWrongClient, func() error { return svc.Reset(ctx, forms, "panel") }},
		{"forms reads the summary", apppref.ErrWrongClient, func() error { _, err := svc.Summaries(ctx, forms); return err }},
		{"unknown app", apppref.ErrUnknownApp, func() error { _, err := svc.Get(ctx, forms, "skymail"); return err }},
		{"unmapped client", apppref.ErrWrongClient, func() error { _, err := svc.Get(ctx, person("frontend-main"), "forms"); return err }},
		{"account center reads forms", apppref.ErrWrongClient, func() error { _, err := svc.Get(ctx, person("account-center"), "forms"); return err }},
		{"account center writes forms", apppref.ErrWrongClient, func() error {
			_, err := svc.Put(ctx, person("account-center"), "forms", "k", value, 1, nil)
			return err
		}},
		{"account center deletes a forms key", apppref.ErrWrongClient, func() error { return svc.DeleteKey(ctx, person("account-center"), "forms", "k") }},
		{"service account", apppref.ErrPersonOnly, func() error {
			_, err := svc.Get(ctx, apppref.Caller{Person: uuid.New(), Client: "skyforms", ServiceAccount: true}, "forms")
			return err
		}},
		{"service account of account center", apppref.ErrPersonOnly, func() error {
			_, err := svc.Summaries(ctx, apppref.Caller{Person: uuid.New(), Client: "account-center", ServiceAccount: true})
			return err
		}},
	} {
		if err := tc.run(); !errors.Is(err, tc.err) {
			t.Fatalf("%s: %v, want %v", tc.name, err, tc.err)
		}
	}

	center := apppref.Caller{Person: forms.Person, Client: "account-center"}
	sums, err := svc.Summaries(ctx, center)
	if err != nil || len(sums) != 1 || sums[0].App != "forms" || sums[0].Keys != 1 || sums[0].UpdatedAt.IsZero() {
		t.Fatalf("summary %+v %v", sums, err)
	}
	if err := svc.Reset(ctx, center, "forms"); err != nil {
		t.Fatal(err)
	}
	if items, err := svc.Get(ctx, forms, "forms"); err != nil || len(items) != 0 {
		t.Fatalf("after reset %+v %v", items, err)
	}
	// Account center's own namespace is an app like any other.
	if _, err := svc.Put(ctx, center, "account", "home", value, 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestServiceOffAnswersOff(t *testing.T) {
	svc := apppref.NewService(apppref.Config{}, apppref.NewMemoryStore())
	if svc.Enabled() {
		t.Fatal("enabled without a client map")
	}
	if _, err := svc.Get(context.Background(), person("skyforms"), "forms"); !errors.Is(err, apppref.ErrOff) {
		t.Fatalf("get: %v", err)
	}
	var none *apppref.Service
	if none.Enabled() || none.Metrics() != nil {
		t.Fatal("a nil service is on")
	}
}

func TestServiceValidatesKeysAndValues(t *testing.T) {
	ctx := context.Background()
	svc := apppref.NewService(testConfig(t), apppref.NewMemoryStore())
	forms := person("skyforms")
	for _, key := range []string{"", ".list", "Forms.list", "forms/list", "forms list", strings.Repeat("k", 65)} {
		if _, err := svc.Put(ctx, forms, "forms", key, json.RawMessage(`1`), 1, nil); !errors.Is(err, apppref.ErrInvalidKey) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
	for _, value := range []string{"", "null", " null ", "{", "{'a':1}", "undefined"} {
		if _, err := svc.Put(ctx, forms, "forms", "k", json.RawMessage(value), 1, nil); !errors.Is(err, apppref.ErrInvalidValue) {
			t.Fatalf("value %q: %v", value, err)
		}
	}
	for _, version := range []int{-1, 1001} {
		if _, err := svc.Put(ctx, forms, "forms", "k", json.RawMessage(`1`), version, nil); !errors.Is(err, apppref.ErrInvalidValue) {
			t.Fatalf("schemaVersion %d: %v", version, err)
		}
	}
	// The limit is on the compact form: spaces the app sent do not count.
	big := `"` + strings.Repeat("a", apppref.MaxValueBytes-2) + `"`
	item, err := svc.Put(ctx, forms, "forms", "big", json.RawMessage("  "+big+"  "), 0, nil)
	if err != nil || string(item.Value) != big || item.SchemaVersion != 1 || item.Version != 1 {
		t.Fatalf("4096 bytes: %v %d %d", err, item.SchemaVersion, item.Version)
	}
	if _, err := svc.Put(ctx, forms, "forms", "big", json.RawMessage(`"`+strings.Repeat("a", apppref.MaxValueBytes-1)+`"`), 1, nil); !errors.Is(err, apppref.ErrValueTooLarge) {
		t.Fatalf("4097 bytes: %v", err)
	}
	item, err = svc.Put(ctx, forms, "forms", "list", json.RawMessage(`{ "details" : false, "sort" : [ "a" ] }`), 3, nil)
	if err != nil || string(item.Value) != `{"details":false,"sort":["a"]}` || item.SchemaVersion != 3 {
		t.Fatalf("compact %q %v", item.Value, err)
	}
}

// 32 keys and 32 KB an app: a new key past either is refused, replacing a
// key counts only its new size, and another app has its own budget.
func TestServiceKeepsTheAppQuota(t *testing.T) {
	ctx := context.Background()
	store := apppref.NewMemoryStore()
	svc := apppref.NewService(testConfig(t), store)
	forms := person("skyforms")
	for i := range apppref.MaxKeysPerApp {
		if _, err := svc.Put(ctx, forms, "forms", "k"+string(rune('a'+i%26))+string(rune('a'+i/26)), json.RawMessage(`1`), 1, nil); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if _, err := svc.Put(ctx, forms, "forms", "one-more", json.RawMessage(`1`), 1, nil); !errors.Is(err, apppref.ErrQuota) {
		t.Fatalf("33rd key: %v", err)
	}
	if _, err := svc.Put(ctx, forms, "forms", "kaa", json.RawMessage(`2`), 1, nil); err != nil {
		t.Fatalf("replacing a key at the key limit: %v", err)
	}
	if err := svc.Reset(ctx, forms, "forms"); err != nil {
		t.Fatal(err)
	}

	value := json.RawMessage(`"` + strings.Repeat("b", apppref.MaxValueBytes-2) + `"`)
	for i := range apppref.MaxBytesPerApp / apppref.MaxValueBytes {
		if _, err := svc.Put(ctx, forms, "forms", "v"+string(rune('a'+i)), value, 1, nil); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
	}
	if _, err := svc.Put(ctx, forms, "forms", "small", json.RawMessage(`1`), 1, nil); !errors.Is(err, apppref.ErrQuota) {
		t.Fatalf("past 32 KB: %v", err)
	}
	if _, err := svc.Put(ctx, forms, "forms", "va", value, 1, nil); err != nil {
		t.Fatalf("replacing at the byte limit: %v", err)
	}
	panel := apppref.Caller{Person: forms.Person, Client: "admin"}
	if _, err := svc.Put(ctx, panel, "panel", "density", json.RawMessage(`"compact"`), 1, nil); err != nil {
		t.Fatalf("another app: %v", err)
	}
}

// Last writer wins without If-Match; with it the stored version must hold.
func TestServiceVersionsAndConflicts(t *testing.T) {
	ctx := context.Background()
	svc := apppref.NewService(testConfig(t), apppref.NewMemoryStore())
	forms := person("skyforms")
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`1`), 1, &apppref.Match{Any: true}); !errors.Is(err, apppref.ErrConflict) {
		t.Fatalf("If-Match * on a missing key: %v", err)
	}
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`1`), 1, &apppref.Match{Version: 1}); !errors.Is(err, apppref.ErrConflict) {
		t.Fatalf("If-Match 1 on a missing key: %v", err)
	}
	first, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`1`), 1, nil)
	if err != nil || first.Version != 1 {
		t.Fatalf("first %+v %v", first, err)
	}
	second, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`2`), 1, &apppref.Match{Version: 1})
	if err != nil || second.Version != 2 {
		t.Fatalf("second %+v %v", second, err)
	}
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`3`), 1, &apppref.Match{Version: 1}); !errors.Is(err, apppref.ErrConflict) {
		t.Fatalf("stale If-Match: %v", err)
	}
	third, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`4`), 1, &apppref.Match{Any: true})
	if err != nil || third.Version != 3 {
		t.Fatalf("If-Match *: %+v %v", third, err)
	}
	items, err := svc.Get(ctx, forms, "forms")
	if err != nil || len(items) != 1 || string(items[0].Value) != "4" || items[0].Version != 3 {
		t.Fatalf("items %+v %v", items, err)
	}
	if err := svc.DeleteKey(ctx, forms, "forms", "list"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteKey(ctx, forms, "forms", "list"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
	if again, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`5`), 1, nil); err != nil || again.Version != 1 {
		t.Fatalf("after delete %+v %v", again, err)
	}
}

func TestServiceRefusesAnInactiveSubjectAndErasesWhileOff(t *testing.T) {
	ctx := context.Background()
	store := apppref.NewMemoryStore()
	svc := apppref.NewService(testConfig(t), store)
	forms := person("skyforms")
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`1`), 1, nil); err != nil {
		t.Fatal(err)
	}
	if got := store.Client(forms.Person, "forms", "list"); got != "skyforms" {
		t.Fatalf("writer client %q", got)
	}
	store.MarkInactive(forms.Person)
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`2`), 1, nil); !errors.Is(err, apppref.ErrSubjectInactive) {
		t.Fatalf("inactive: %v", err)
	}
	off := apppref.NewService(apppref.Config{}, store)
	if n, err := off.EraseSubject(ctx, forms.Person); err != nil || n != 1 {
		t.Fatalf("erase while off: %d %v", n, err)
	}
}

// The metrics name the app only when it is configured, count outcomes and
// value sizes, and never carry a person or a value.
func TestMetricsCarryNoPersonOrValue(t *testing.T) {
	ctx := context.Background()
	svc := apppref.NewService(testConfig(t), apppref.NewMemoryStore())
	forms := person("skyforms")
	if _, err := svc.Put(ctx, forms, "forms", "list", json.RawMessage(`{"secret":"gizli-deger"}`), 1, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.Get(ctx, forms, "panel")
	_, _ = svc.Get(ctx, forms, "kisi-adi")
	_, _ = svc.Summaries(ctx, forms)
	svc.Metrics().Record("forms", apppref.OpPut, "rate_limited")
	text := svc.Metrics().Prometheus()
	for _, want := range []string{
		`skylab_app_preferences_requests_total{app="forms",op="put",outcome="ok"} 1`,
		`skylab_app_preferences_requests_total{app="forms",op="put",outcome="rate_limited"} 1`,
		`skylab_app_preferences_requests_total{app="panel",op="get",outcome="wrong_client"} 1`,
		`skylab_app_preferences_requests_total{app="other",op="get",outcome="unknown_app"} 1`,
		`skylab_app_preferences_requests_total{app="all",op="summary",outcome="wrong_client"} 1`,
		`skylab_app_preferences_value_bytes_bucket{app="forms",le="64"} 1`,
		`skylab_app_preferences_value_bytes_bucket{app="forms",le="+Inf"} 1`,
		`skylab_app_preferences_value_bytes_sum{app="forms"} 24`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Fatalf("metrics lack %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{forms.Person.String(), "gizli-deger", "kisi-adi", "skyforms"} {
		if strings.Contains(text, leak) {
			t.Fatalf("metrics carry %q:\n%s", leak, text)
		}
	}
}
