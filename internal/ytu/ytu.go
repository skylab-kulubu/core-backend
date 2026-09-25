// Package ytu turns what the YTÜ Microsoft login says about a person into the
// university, department and faculty core keeps on the User shadow.
//
// Keycloak copies two attributes from the YTÜ Microsoft (OBS) login:
// `university` (a fixed "Yıldız Teknik Üniversitesi") and `department` (from
// Microsoft Graph). The client scope `department_ve_university_to_jwt` puts
// them into tokens. Microsoft sends no faculty, so it is derived here.
//
// The `department` text comes in three shapes (production, 2026-09-24):
//   - a department name ("Bilgisayar Mühendisliği");
//   - a name written without spaces ("MatematikMühendisliği");
//   - the three-character program code of the YTÜ student number ("011").
//
// Sources, all read 2026-09-25 (details: notes/research-ytu-department-codes.md
// in the platform workspace):
//   - program codes: ÖİDB "Dereceye Giren Öğrenciler" lists 2022-23 to 2025-26
//     (student number, faculty and department on one row), for example
//     https://ogi.yildiz.edu.tr/sites/ogi.yildiz.edu.tr/files/ytu-2025-2026-egitim-ogretim-yili-dereceye-giren-ogrenciler.xlsx,
//     and the Physics department's FIZ1001/FIZ1002 timetables that print
//     "(011) - BİLGİSAYAR MÜHENDİSLİĞİ", for example
//     https://fzk.yildiz.edu.tr/sites/fzk.yildiz.edu.tr/files/2026-09/2026-2027-guz-fiz1001-fizik1-haftalik-ders-programi_23.09.2026.pdf;
//     `018` is inferred (FIZ1002 2025-26 final seating: group 53 is Kimya
//     (İng) `02D` plus Yapay Zeka ve Veri Müh. (İng)), the others are verified;
//   - faculties: Cumhurbaşkanı Kararı 11681, Resmî Gazete 31.08.2026 sayı 33356
//     (https://www.resmigazete.gov.tr/eskiler/2026/08/20260831-1.pdf) founds the
//     Bilgisayar ve Bilişim Bilimleri Fakültesi and closes the Uygulamalı
//     Bilimler Fakültesi; YTÜ's announcement
//     (https://yildiz.edu.tr/universite/haberler/ytu-bilgisayar-ve-bilisim-bilimleri-fakultesi-kuruldu)
//     and faculty page (https://yildiz.edu.tr/egitim/akademik-birimler/fakulteler)
//     move Bilgisayar, Yapay Zeka ve Veri and Matematik Mühendisliği into it;
//     every other department keeps its faculty (YÖK Atlas 2026,
//     https://yokatlas.yok.gov.tr/detay/<ÖSYM kodu>).
package ytu

import (
	"regexp"
	"strings"
	"unicode"
)

// Profile is the university, department and faculty a YTÜ-linked person
// carries on the User shadow.
type Profile struct {
	University string
	Department string
	Faculty    string
}

// FromClaims reads the `university` and `department` attributes the YTÜ
// Microsoft login writes. A person is YTÜ-linked when the university is
// present; the department is then authoritative too, so an absent or
// unknown department clears the stored one rather than keeping an older
// value.
func FromClaims(university, department string) (Profile, bool) {
	university = clean(university)
	if university == "" {
		return Profile{}, false
	}
	d := Department(department)
	return Profile{University: university, Department: d, Faculty: Faculty(d)}, true
}

// Department returns the department name for a raw `department` value: a
// known program code becomes its department name, an unknown code becomes
// empty (a bare code means nothing to a reader), a known name is written the
// way YTÜ writes it, and any other name is kept with its spaces tidied.
// "(İngilizce)" programs keep the suffix.
func Department(raw string) string {
	raw = clean(raw)
	if programCode.MatchString(strings.ToUpper(raw)) {
		return codes[strings.ToUpper(raw)]
	}
	base, suffix := splitSuffix(raw)
	name, ok := departments[key(base)]
	if !ok {
		return raw
	}
	if suffix == "" {
		return name
	}
	if key(suffix) == key(english) {
		suffix = english
	}
	return name + " " + suffix
}

// Faculty returns the current faculty of a department name, or empty when
// the department is not in the table. A program in another language, such as
// "(İngilizce)", shares its department's faculty.
func Faculty(department string) string {
	base, _ := splitSuffix(clean(department))
	name, ok := departments[key(base)]
	if !ok {
		return ""
	}
	return faculties[name]
}

// Departments lists every department name the table knows.
func Departments() []string {
	out := make([]string, 0, len(faculties))
	for name := range faculties {
		out = append(out, name)
	}
	return out
}

const english = "(İngilizce)"

// A YTÜ program code is three characters, digits or capitals, starting with a
// digit: `011`, `02D`, `0A1`.
var programCode = regexp.MustCompile(`^[0-9][0-9A-Z]{2}$`)

// codes are the program codes seen in production `department` values plus
// `018`, each mapped to its department name. Other codes stay unknown on
// purpose until one shows up and is checked.
var codes = map[string]string{
	"011": "Bilgisayar Mühendisliği",
	"018": "Yapay Zeka ve Veri Mühendisliği", // inferred; taught only in English, and Microsoft sends this name without the suffix
	"022": "Fizik",
	"023": "İstatistik",
	"02D": "Kimya (İngilizce)",
	"034": "Siyaset Bilimi ve Uluslararası İlişkiler",
	"035": "İktisat (İngilizce)",
	"052": "Matematik Mühendisliği",
	"058": "Matematik Mühendisliği (İngilizce)",
	"065": "Makine Mühendisliği",
	"091": "Bilgisayar ve Öğretim Teknolojileri Eğitimi", // program: BÖT Öğretmenliği
}

const (
	bbbf  = "Bilgisayar ve Bilişim Bilimleri Fakültesi"
	egitm = "Eğitim Fakültesi"
	eef   = "Elektrik-Elektronik Fakültesi"
	fef   = "Fen-Edebiyat Fakültesi"
	gidf  = "Gemi İnşaatı ve Denizcilik Fakültesi"
	iibf  = "İktisadi ve İdari Bilimler Fakültesi"
	insf  = "İnşaat Fakültesi"
	kmf   = "Kimya-Metalurji Fakültesi"
	makf  = "Makine Fakültesi"
	mimf  = "Mimarlık Fakültesi"
	stf   = "Sanat ve Tasarım Fakültesi"
)

// faculties maps each YTÜ department or program name to its faculty as of
// 2026-08-31. Havacılık Elektroniği is left out: its faculty was closed and
// its new home is not published.
var faculties = map[string]string{
	"Bilgisayar Mühendisliği":         bbbf,
	"Yapay Zeka ve Veri Mühendisliği": bbbf,
	"Matematik Mühendisliği":          bbbf,
	"Siber Güvenlik Mühendisliği":     bbbf,

	"Biyomedikal Mühendisliği":              eef,
	"Elektrik Mühendisliği":                 eef,
	"Elektronik ve Haberleşme Mühendisliği": eef,
	"Kontrol ve Otomasyon Mühendisliği":     eef,

	"Fizik":                             fef,
	"İstatistik":                        fef,
	"Kimya":                             fef,
	"Matematik":                         fef,
	"Moleküler Biyoloji ve Genetik":     fef,
	"Türk Dili ve Edebiyatı":            fef,
	"Fransızca Mütercim ve Tercümanlık": fef,

	"İktisat": iibf,
	"İşletme": iibf,
	"Siyaset Bilimi ve Uluslararası İlişkiler": iibf,

	"Çevre Mühendisliği":  insf,
	"Harita Mühendisliği": insf,
	"İnşaat Mühendisliği": insf,

	"Biyomühendislik":                   kmf,
	"Gıda Mühendisliği":                 kmf,
	"Kimya Mühendisliği":                kmf,
	"Metalurji ve Malzeme Mühendisliği": kmf,

	"Endüstri Mühendisliği":   makf,
	"Makine Mühendisliği":     makf,
	"Mekatronik Mühendisliği": makf,

	"Kültür Varlıklarını Koruma ve Onarım": mimf,
	"Mimarlık":                mimf,
	"Şehir ve Bölge Planlama": mimf,

	"Bileşik Sanatlar":         stf,
	"Fotoğraf ve Video":        stf,
	"Grafik Tasarımı":          stf,
	"İletişim ve Tasarımı":     stf,
	"Müzik Toplulukları":       stf,
	"Sanat ve Kültür Yönetimi": stf,
	"Ses Sanatları Tasarımı":   stf,

	"Bilgisayar ve Öğretim Teknolojileri Eğitimi":      egitm,
	"Bilgisayar ve Öğretim Teknolojileri Öğretmenliği": egitm,
	"Matematik ve Fen Bilimleri Eğitimi":               egitm,
	"Fen Bilgisi Öğretmenliği":                         egitm,
	"İlköğretim Matematik Öğretmenliği":                egitm,
	"İngilizce Öğretmenliği":                           egitm,
	"Okul Öncesi Öğretmenliği":                         egitm,
	"Rehberlik ve Psikolojik Danışmanlık":              egitm,
	"Sınıf Öğretmenliği":                               egitm,
	"Sosyal Bilgiler Öğretmenliği":                     egitm,
	"Türkçe Öğretmenliği":                              egitm,

	"Gemi İnşaatı ve Gemi Makineleri Mühendisliği": gidf,
	"Gemi Makineleri İşletme Mühendisliği":         gidf,
}

// departments finds a table name by its spelling-insensitive key, so
// "MatematikMühendisliği", "BİLGİSAYAR MÜHENDİSLİĞİ" and "Yapay Zekâ ..."
// resolve to the table's spelling. Alternative spellings YTÜ itself uses are
// listed as aliases.
var departments = func() map[string]string {
	out := make(map[string]string, len(faculties)+1)
	for name := range faculties {
		out[key(name)] = name
	}
	out[key("Metalürji ve Malzeme Mühendisliği")] = "Metalurji ve Malzeme Mühendisliği"
	return out
}()

// clean trims the text and collapses every run of whitespace to one space.
func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitSuffix separates a trailing parenthesised qualifier such as
// "(İngilizce)" or "(%30 İngilizce)" from the department name.
func splitSuffix(s string) (base, suffix string) {
	if !strings.HasSuffix(s, ")") {
		return s, ""
	}
	open := strings.LastIndex(s, "(")
	if open <= 0 {
		return s, ""
	}
	return strings.TrimSpace(s[:open]), s[open:]
}

// key folds a name for lookup: no whitespace, Turkish lower case, and the
// circumflex dropped (Zekâ and Zeka are the same word).
func key(s string) string {
	s = strings.ToLowerSpecial(unicode.TurkishCase, s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			continue
		case r == 'â':
			r = 'a'
		case r == 'î':
			r = 'i'
		case r == 'û':
			r = 'u'
		}
		b.WriteRune(r)
	}
	return b.String()
}
