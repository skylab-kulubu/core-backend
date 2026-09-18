package event

import "testing"

func TestDecodeExtraFormArrayDefault(t *testing.T) {
	t.Parallel()
	alias, extra, err := DecodeExtraForm([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if alias != "" || len(extra) != 0 {
		t.Fatalf("alias %q extra %+v", alias, extra)
	}
}

func TestEncodeDecodeExtraFormRoundTrip(t *testing.T) {
	t.Parallel()
	raw, err := EncodeExtraForm("skydays2026", []EventFormLink{{
		Label: "CTF",
		URL:   "https://forms.example.test/ctf",
		Alias: "skydays-ctf2026",
	}})
	if err != nil {
		t.Fatal(err)
	}
	alias, extra, err := DecodeExtraForm(raw)
	if err != nil {
		t.Fatal(err)
	}
	if alias != "skydays2026" {
		t.Fatalf("alias %q", alias)
	}
	if len(extra) != 1 || extra[0].Label != "CTF" || extra[0].URL != "https://forms.example.test/ctf" || extra[0].Alias != "skydays-ctf2026" {
		t.Fatalf("extra %+v", extra)
	}
}
