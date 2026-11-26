package formsfeed

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A guest's optional details from a form mapping's answers
// (docs/form-mappings.md#the-details): the phone number, the university,
// the department and the grade (year of study), the columns a Walk-in fills
// (ticket.WalkIn). An answer is a stranger's text, so a detail that does
// not read is left out, never the guest.

// Detail names, for the counts of the details left out (Details.Dropped).
const (
	DetailPhone      = "phone"
	DetailUniversity = "university"
	DetailDepartment = "department"
	DetailGrade      = "grade"
)

// Details are a guest's optional details as a mapping's answers give them,
// each "" for none. Dropped names the details answered but left out: a
// phone that is no number, a university or department over
// ticket.MaxWalkInText characters or with a control character other than a
// tab or a line break (those fold into a space, as every run of spaces).
type Details struct {
	Phone      string
	University string
	Department string
	Grade      string
	Dropped    []string
}

// MappedDetails reads the details of m's questions from answers:
//
//   - the phone in the Walk-in's one form (ticket.NormalizePhone);
//   - the university and the department trimmed, inner spaces, tabs and line
//     breaks folded into one space;
//   - the grade as one of ticket.Grades (GradeOf).
func MappedDetails(m Mapping, answers map[string]string) Details {
	var d Details
	if raw := answerOf(m.Phone, answers); raw != "" {
		if phone, ok := ticket.NormalizePhone(raw); ok {
			d.Phone = phone
		} else {
			d.Dropped = append(d.Dropped, DetailPhone)
		}
	}
	text := func(id, name string) string {
		raw := answerOf(id, answers)
		if raw == "" {
			return ""
		}
		v := cleanName(raw)
		if v == "" || utf8.RuneCountInString(v) > ticket.MaxWalkInText {
			d.Dropped = append(d.Dropped, name)
			return ""
		}
		return v
	}
	d.University = text(m.University, DetailUniversity)
	d.Department = text(m.Department, DetailDepartment)
	d.Grade = GradeOf(answerOf(m.Grade, answers))
	return d
}

func answerOf(id string, answers map[string]string) string {
	if id == "" {
		return ""
	}
	return strings.TrimSpace(answers[id])
}

// withDetails puts d on g where g has none of its own.
func withDetails(g ticket.GuestInfo, d Details) ticket.GuestInfo {
	for _, f := range []struct {
		stored *string
		v      string
	}{{&g.PhoneNumber, d.Phone}, {&g.University, d.University}, {&g.Department, d.Department}, {&g.Grade, d.Grade}} {
		if *f.stored == "" {
			*f.stored = f.v
		}
	}
	return g
}

// detailsOf merges the details of the mappings that name any: for each,
// the first mapping's (they are read in their Events' order) that answers
// it.
func detailsOf(mappings []Mapping, answers map[string]string) Details {
	var out Details
	for _, m := range mappings {
		if !m.HasDetails() {
			continue
		}
		d := MappedDetails(m, answers)
		g := withDetails(ticket.GuestInfo{PhoneNumber: out.Phone, University: out.University, Department: out.Department, Grade: out.Grade}, d)
		out.Phone, out.University, out.Department, out.Grade = g.PhoneNumber, g.University, g.Department, g.Grade
	}
	return out
}

var (
	gradeYear = regexp.MustCompile(`\b([0-9])\b`)
	// gradeWords are years written out, Turkish and English.
	gradeWords = map[string]string{
		"birinci": "1", "ikinci": "2", "ucuncu": "3", "dorduncu": "4", "besinci": "5", "altinci": "6",
		"first": "1", "second": "2", "third": "3", "fourth": "4", "fifth": "5", "sixth": "6",
		"1st": "1", "2nd": "2", "3rd": "3", "4th": "4", "5th": "5", "6th": "6",
	}
)

// GradeOf reads a grade answer ("3. Sınıf", "Hazırlık", "Yüksek Lisans",
// a multiple choice's "1. Sınıf, 2. Sınıf"…) as one of ticket.Grades:
//
//   - prep: hazırlık, prep;
//   - 1 to 4: the one year it names, in digits or words (birinci,
//     ikinci…);
//   - masters: yüksek lisans, master, msc; phd: doktora, phd, doctorate.
//     Either wins over a year ("yüksek lisans 1. sınıf");
//   - graduate: mezun, graduate (not undergraduate);
//   - other: anything else answered, a year past 4, or more than one of
//     them.
//
// "" for no answer. The answer itself is not kept (Yusuf, 2026-11-26): the
// column holds the key alone; a free text is a stranger's and nothing reads
// it.
func GradeOf(answer string) string {
	t := foldAnswer(answer)
	if t == "" {
		return ""
	}
	found := map[string]bool{}
	padded := " " + t + " "
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(padded, " "+w+" ") {
				return true
			}
		}
		return false
	}
	if has("hazirlik", "prep", "preparatory") {
		found["prep"] = true
	}
	if has("yuksek lisans", "master", "masters", "msc", "yl", "tezli", "tezsiz") {
		found["masters"] = true
	}
	if has("doktora", "phd", "ph d", "doctorate", "doctoral") {
		found["phd"] = true
	}
	if has("mezun", "graduate", "graduated", "alumni") && !has("undergraduate") {
		found["graduate"] = true
	}
	years := map[string]bool{}
	for _, m := range gradeYear.FindAllStringSubmatch(t, -1) {
		years[m[1]] = true
	}
	for _, w := range strings.Fields(t) {
		if y, ok := gradeWords[w]; ok {
			years[y] = true
		}
	}
	switch {
	case found["masters"] && !found["phd"]:
		return "masters"
	case found["phd"] && !found["masters"]:
		return "phd"
	case found["masters"] || found["phd"]:
		return "other"
	}
	if len(years) == 1 {
		for y := range years {
			if y >= "1" && y <= "4" {
				found[y] = true
			} else {
				return "other"
			}
		}
	} else if len(years) > 1 {
		return "other"
	}
	if len(found) != 1 {
		return "other"
	}
	for k := range found {
		return k
	}
	return "other"
}

// foldAnswer is the answer in lower case the Turkish way, its Turkish
// letters without their marks, anything but ASCII letters and digits a
// space, spaces folded.
func foldAnswer(s string) string {
	s = strings.ToLowerSpecial(unicode.TurkishCase, s)
	var b strings.Builder
	for _, r := range s {
		if plain, ok := turkishPlain[r]; ok {
			r = plain
		}
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// turkishPlain are the Turkish letters (and the circumflexed vowels) in
// ASCII.
var turkishPlain = map[rune]rune{
	'ı': 'i', 'ş': 's', 'ğ': 'g', 'ü': 'u', 'ö': 'o', 'ç': 'c', 'â': 'a', 'î': 'i', 'û': 'u',
}
