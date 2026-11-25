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
			name: "skyforms excel: headers with a colon and a possessive ending",
			text: "Ad-Soyad:\tTelefon Numaran:\tE-mail adresin:\tÖğrenim Durumun\n" +
				"Ayşe Nur Yılmaz\t05550000000\tAyse@Example.com\tLisans\n",
			delimiter: "tab", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe Nur", lastName: "Yılmaz", email: "ayse@example.com"}},
		},
		{
			name:      "possessive and question headers",
			text:      "Adın?;Soyadın?;E-posta adresin*\nAli;Veli;ali@example.com\n",
			delimiter: "semicolon", header: true,
			want: []importLine{{line: 2, firstName: "Ali", lastName: "Veli", email: "ali@example.com"}},
		},
		{
			name:      "İsminiz Soyisminiz and an e-mail question",
			text:      "İsminiz Soyisminiz,Your email\nAda Lovelace,ada@example.com\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ada", lastName: "Lovelace", email: "ada@example.com"}},
		},
		{
			name:      "a column that only mentions e-mail is not the e-mail",
			text:      "İsim Soyisim,E-posta,E-posta ile haber verelim mi?\nAda Lovelace,ada@example.com,Evet\n",
			delimiter: "comma", header: true,
			want: []importLine{{line: 2, firstName: "Ada", lastName: "Lovelace", email: "ada@example.com"}},
		},
		{
			name: "microsoft forms export: a question that mentions e-mail leaves the respondent's addresses",
			text: "Kimlik;Başlangıç saati;Tamamlanma saati;E-posta;Ad;Mail ile duyuru ister misin?\n" +
				"1;25.11.2026 10:00;25.11.2026 10:02;ayse@std.yildiz.edu.tr;Ayşe Yılmaz;Evet\n",
			delimiter: "semicolon", header: true,
			want: []importLine{{line: 2, firstName: "Ayşe", lastName: "Yılmaz", email: "ayse@std.yildiz.edu.tr"}},
		},
		{
			name:      "an address in the first row is not a header",
			text:      "Ali,Veli,ali@gmail.com\n",
			delimiter: "comma", header: false,
			want: []importLine{{line: 1, firstName: "Ali", lastName: "Veli", email: "ali@gmail.com"}},
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

// Two columns holding addresses are refused, naming both: core does not
// guess which is the e-mail.
func TestParseImportAmbiguousEmail(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"e-posta and okul e-postası": {"Ad Soyad,E-posta,Okul e-postası\nAda Lovelace,ada@example.com,ada@std.yildiz.edu.tr\n", []string{"E-posta", "Okul e-postası"}},
		"two exact names":            {"Email,E-posta\nx@example.com,a@example.com\n", []string{"Email", "E-posta"}},
		"microsoft forms, two own questions": {
			"ID,Start time,Completion time,Email,Name,E-mail,School e-mail\n1,a,b,anonymous,anonymous,a@example.com,a@std.yildiz.edu.tr\n",
			[]string{"E-mail", "School e-mail"},
		},
	} {
		_, _, err := parseImport(tc.text)
		var amb *ImportAmbiguousEmailError
		if !errors.Is(err, ErrImportAmbiguousEmail) || !errors.As(err, &amb) || strings.Join(amb.Columns, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClassifyHeader(t *testing.T) {
	t.Parallel()
	for cell, want := range map[string]columnKind{
		"E-mail adresin:": colEmail, "Email": colEmail, "e-posta": colEmail, "EPOSTA": colEmail, "Mail": colEmail,
		"Okul e-postası": colEmail, "E-posta adresiniz?": colEmail,
		"Ad-Soyad:": colFull, "Adınız Soyadınız": colFull, "İsim Soyisim": colFull, "Adın Soyadın*": colFull, "Adınız Soyadı": colFull, "İsminiz Soyisminiz": colFull,
		"Adınız": colFirst, "Ad": colFirst, "Soyadınız": colLast, "Oturumun": colSession,
		"Telefon Numaran:": colNone, "Öğrenim Durumun": colNone, "Adresin": colNone, "ali@gmail.com": colNone, "": colNone,
	} {
		if got := classifyHeader(cell); got != want {
			t.Errorf("%q: %v, want %v", cell, got, want)
		}
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
