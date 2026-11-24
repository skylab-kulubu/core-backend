package ticket

import (
	"errors"
	"strings"
	"testing"
)

// The list's shapes the import reads (docs/attendee-import.md): a header in
// Turkish or English, Excel's semicolons, a pasted spreadsheet's tabs, a
// Microsoft Forms export, no header at all.
func TestParseImportShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		text      string
		delimiter string
		header    bool
		want      []importLine
	}{
		{
			name:      "turkish header, comma",
			text:      "Ad,Soyad,E-posta,Oturum\nAyşe,Yılmaz,AYSE@Example.com,Açılış\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe", lastName: "Yılmaz", email: "ayse@example.com", sessions: []string{"Açılış"}}},
		},
		{
			name:      "excel semicolons with a byte order mark and CRLF",
			text:      "\ufeffADI;SOYADI;E-posta Adresi\r\nAli;Veli;ali@example.com\r\n",
			delimiter: "semicolon", header: true,
			want: []importLine{{line: 2, firstName: "Ali", lastName: "Veli", email: "ali@example.com"}},
		},
		{
			name:      "pasted from a spreadsheet, no header",
			text:      "Ali\tVeli\tali@example.com\n\nCan\tDemir\tcan@example.com\tAtölye A; Atölye B\n",
			delimiter: "tab", header: false,
			want: []importLine{
				{line: 1, firstName: "Ali", lastName: "Veli", email: "ali@example.com"},
				{line: 3, firstName: "Can", lastName: "Demir", email: "can@example.com", sessions: []string{"Atölye A", "Atölye B"}},
			},
		},
		{
			name:      "no header, the e-mail first",
			text:      "ali@example.com,Ali,Veli\n",
			delimiter: "comma", header: false,
			want: []importLine{{line: 1, firstName: "Ali", lastName: "Veli", email: "ali@example.com"}},
		},
		{
			name: "microsoft forms export: the form's questions win over the respondent columns",
			text: "ID,Start time,Completion time,Email,Name,Adınız Soyadınız,E-posta adresiniz\n" +
				"1,1/1/25,1/1/25,anonymous,anonymous,Ayşe Nur Yılmaz,ayse@example.com\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe Nur", lastName: "Yılmaz", email: "ayse@example.com"}},
		},
		{
			name: "microsoft forms export in English: the form's first and last name questions",
			text: "ID,Start time,Completion time,Email,Name,First name,Last name,E-mail address\n" +
				"1,1/1/25 10:00,1/1/25 10:02,anonymous,anonymous,Grace,Hopper,grace@example.com\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Grace", lastName: "Hopper", email: "grace@example.com"}},
		},
		{
			name: "microsoft forms export in Turkish: the respondent's Ad and E-posta give way to the questions",
			text: "Kimlik;Başlangıç saati;Tamamlanma saati;E-posta;Ad;Adınız;Soyadınız;E-posta adresiniz\n" +
				"1;25.11.2026 10:00;25.11.2026 10:02;ogrenci@std.yildiz.edu.tr;Öğrenci Hesabı;Ayşe;Yılmaz;ayse@example.com\n",
			delimiter: "semicolon", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe", lastName: "Yılmaz", email: "ayse@example.com"}},
		},
		{
			name: "microsoft forms export in Turkish without questions: the respondent's Ad is a full name",
			text: "Kimlik;Başlangıç saati;Tamamlanma saati;E-posta;Ad;Hangi oturum?\n" +
				"1;25.11.2026 10:00;25.11.2026 10:02;ayse@std.yildiz.edu.tr;Ayşe Nur Yılmaz;x\n",
			delimiter: "semicolon", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe Nur", lastName: "Yılmaz", email: "ayse@std.yildiz.edu.tr"}},
		},
		{
			name:      "microsoft forms export in English without questions",
			text:      "ID,Start time,Completion time,Email,Name\n1,a,b,ada@example.com,Ada Lovelace\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ada", lastName: "Lovelace", email: "ada@example.com"}},
		},
		{
			name:      "a byte that is not UTF-8 stays in its row",
			text:      "ad;soyad;eposta\nAy\xfee;Kaya;ayse@example.com\nAli;Veli;ali@example.com\n",
			delimiter: "semicolon", header: true,
			want: []importLine{
				{line: 2, firstName: "Ay\uFFFDe", lastName: "Kaya", email: "ayse@example.com"},
				{line: 3, firstName: "Ali", lastName: "Veli", email: "ali@example.com"},
			},
		},
		{
			name:      "quoted cells and extra blanks",
			text:      "first name,last name,email\n\"  Ada  \",\"Love lace\",\" ada@example.com \"\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ada", lastName: "Love lace", email: "ada@example.com"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cols, err := parseImport(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if cols.Delimiter != tc.delimiter || cols.Header != tc.header || cols.Email == "" {
				t.Fatalf("columns %+v", cols)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("rows %+v", got)
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if g.line != w.line || g.firstName != w.firstName || g.lastName != w.lastName || g.email != w.email ||
					strings.Join(g.sessions, "|") != strings.Join(w.sessions, "|") {
					t.Fatalf("row %d: got %+v want %+v", i, g, w)
				}
			}
		})
	}
}

func TestParseImportRefusals(t *testing.T) {
	t.Parallel()
	many := "ad,soyad,eposta\n" + strings.Repeat("a,b,c@example.com\n", MaxImportRows+1)
	for _, tc := range []struct {
		name string
		text string
		want error
	}{
		{"empty", "  \n\n", ErrImportEmpty},
		{"header only", "ad,soyad,eposta\n", ErrImportEmpty},
		{"too many rows", many, ErrImportTooManyRows},
		{"too large", strings.Repeat("x", MaxImportBytes+1), ErrImportTooLarge},
		{"no e-mail column", "ad,soyad\nAli,Veli\n", ErrImportNoEmail},
	} {
		if _, _, err := parseImport(tc.text); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// Exactly the limit is taken.
	if rows, _, err := parseImport("ad,soyad,eposta\n" + strings.Repeat("a,b,c@example.com\n", MaxImportRows)); err != nil || len(rows) != MaxImportRows {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
}

func TestBadCharacters(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{"Ayşe": false, "Nul\x00": true, "Bell\a": true, "Ay\uFFFDe": true, "": false} {
		if got := badCharacters(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}

func TestValidImportEmail(t *testing.T) {
	t.Parallel()
	for address, want := range map[string]bool{
		"ada@example.com":                true,
		"ada.lovelace@std.yildiz.edu.tr": true,
		"ada@localhost":                  false,
		"ada example@example.com":        false,
		"Ada <ada@example.com>":          false,
		"ada@example.":                   false,
		"":                               false,
		"@example.com":                   false,
	} {
		if got := validImportEmail(address); got != want {
			t.Errorf("%q: %v", address, got)
		}
	}
}
