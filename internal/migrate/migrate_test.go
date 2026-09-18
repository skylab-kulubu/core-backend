package migrate

import (
	"strings"
	"testing"
)

func TestVersionsIncludeExtraFormURLsInOrder(t *testing.T) {
	t.Parallel()
	got, err := Versions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no migrations")
	}
	found := false
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("not increasing %v", got)
		}
	}
	for _, v := range got {
		if v == 20260918180000 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing extra_form_urls version in %v", got)
	}
}

func TestExtraFormSQLIsIdempotent(t *testing.T) {
	t.Parallel()
	sql, err := ExtraFormSQL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS extra_form_urls") {
		t.Fatalf("sql %s", sql)
	}
}
