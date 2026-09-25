package ytu_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

const (
	bbbf  = "Bilgisayar ve Bilişim Bilimleri Fakültesi"
	egitm = "Eğitim Fakültesi"
	eef   = "Elektrik-Elektronik Fakültesi"
	fef   = "Fen-Edebiyat Fakültesi"
	iibf  = "İktisadi ve İdari Bilimler Fakültesi"
	insf  = "İnşaat Fakültesi"
	kmf   = "Kimya-Metalurji Fakültesi"
	makf  = "Makine Fakültesi"
	mimf  = "Mimarlık Fakültesi"
)

// Every distinct Keycloak `department` value seen in production on
// 2026-09-24 (29 values: 17 names, 2 written without spaces, 10 program
// codes), with the department and faculty core stores for it.
func TestProductionDepartmentValues(t *testing.T) {
	t.Parallel()
	cases := []struct{ raw, department, faculty string }{
		{"Bilgisayar Mühendisliği", "Bilgisayar Mühendisliği", bbbf},
		{"Yapay Zeka ve Veri Mühendisliği", "Yapay Zeka ve Veri Mühendisliği", bbbf},
		{"Matematik Mühendisliği", "Matematik Mühendisliği", bbbf},
		{"Matematik Mühendisliği (İngilizce)", "Matematik Mühendisliği (İngilizce)", bbbf},
		{"Bilgisayar ve Öğretim Teknolojileri Eğitimi", "Bilgisayar ve Öğretim Teknolojileri Eğitimi", egitm},
		{"Matematik ve Fen Bilimleri Eğitimi", "Matematik ve Fen Bilimleri Eğitimi", egitm},
		{"Elektrik Mühendisliği", "Elektrik Mühendisliği", eef},
		{"Elektronik ve Haberleşme Mühendisliği", "Elektronik ve Haberleşme Mühendisliği", eef},
		{"İstatistik", "İstatistik", fef},
		{"Matematik", "Matematik", fef},
		{"İktisat", "İktisat", iibf},
		{"Harita Mühendisliği", "Harita Mühendisliği", insf},
		{"İnşaat Mühendisliği (İngilizce)", "İnşaat Mühendisliği (İngilizce)", insf},
		{"Metalurji ve Malzeme Mühendisliği", "Metalurji ve Malzeme Mühendisliği", kmf},
		{"Mekatronik Mühendisliği", "Mekatronik Mühendisliği", makf},
		{"Mimarlık", "Mimarlık", mimf},
		{"Şehir ve Bölge Planlama", "Şehir ve Bölge Planlama", mimf},

		{"MatematikMühendisliği", "Matematik Mühendisliği", bbbf},
		{"YapayZekaveVeriMühendisliği", "Yapay Zeka ve Veri Mühendisliği", bbbf},

		{"011", "Bilgisayar Mühendisliği", bbbf},
		{"052", "Matematik Mühendisliği", bbbf},
		{"058", "Matematik Mühendisliği (İngilizce)", bbbf},
		{"022", "Fizik", fef},
		{"023", "İstatistik", fef},
		{"02D", "Kimya (İngilizce)", fef},
		{"034", "Siyaset Bilimi ve Uluslararası İlişkiler", iibf},
		{"035", "İktisat (İngilizce)", iibf},
		{"065", "Makine Mühendisliği", makf},
		{"091", "Bilgisayar ve Öğretim Teknolojileri Eğitimi", egitm},
	}
	if len(cases) != 29 {
		t.Fatalf("the production snapshot has 29 values, the table has %d", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got, linked := ytu.FromClaims("Yıldız Teknik Üniversitesi", tc.raw)
			if !linked {
				t.Fatal("a token with the university claim is YTÜ-linked")
			}
			want := ytu.Profile{University: "Yıldız Teknik Üniversitesi", Department: tc.department, Faculty: tc.faculty}
			if got != want {
				t.Fatalf("FromClaims(%q) = %+v, want %+v", tc.raw, got, want)
			}
		})
	}
}

func TestDepartment(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, raw, want string }{
		{"inferred code 018", "018", "Yapay Zeka ve Veri Mühendisliği"},
		{"lower-case code", "02d", "Kimya (İngilizce)"},
		{"code with spaces around", " 011 ", "Bilgisayar Mühendisliği"},
		{"unknown code is dropped, not stored raw", "012", ""},
		{"unknown lettered code is dropped", "0B1", ""},
		{"blank", "   ", ""},
		{"empty", "", ""},
		{"extra inner spaces", "  Bilgisayar   Mühendisliği ", "Bilgisayar Mühendisliği"},
		{"upper case", "BİLGİSAYAR MÜHENDİSLİĞİ", "Bilgisayar Mühendisliği"},
		{"circumflex spelling", "Yapay Zekâ ve Veri Mühendisliği", "Yapay Zeka ve Veri Mühendisliği"},
		{"space-less English variant", "MatematikMühendisliği(İngilizce)", "Matematik Mühendisliği (İngilizce)"},
		{"English variant of a known name", "Kimya Mühendisliği (İngilizce)", "Kimya Mühendisliği (İngilizce)"},
		{"unknown name is kept as written", "Uzay Bilimleri", "Uzay Bilimleri"},
		{"unknown name keeps its own casing", "uzay  bilimleri", "uzay bilimleri"},
		{"new Siber Güvenlik department", "Siber Güvenlik Mühendisliği", "Siber Güvenlik Mühendisliği"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ytu.Department(tc.raw); got != tc.want {
				t.Fatalf("Department(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestFaculty(t *testing.T) {
	t.Parallel()
	cases := []struct{ department, want string }{
		{"Siber Güvenlik Mühendisliği", bbbf},
		{"Yapay Zeka ve Veri Mühendisliği (İngilizce)", bbbf},
		{"Kimya", fef},
		{"Kimya Mühendisliği", kmf},
		{"Kimya Mühendisliği (İngilizce)", kmf},
		{"İktisat (%30 İngilizce)", iibf},
		{"Endüstri Mühendisliği", makf},
		{"Gemi İnşaatı ve Gemi Makineleri Mühendisliği", "Gemi İnşaatı ve Denizcilik Fakültesi"},
		{"Grafik Tasarımı", "Sanat ve Tasarım Fakültesi"},
		{"İlköğretim Matematik Öğretmenliği", egitm},
		{"Bilgisayar ve Öğretim Teknolojileri Öğretmenliği", egitm},
		{"Biyomedikal Mühendisliği", eef},
		// Its faculty was closed on 2026-08-31 and where the program went is unknown.
		{"Havacılık Elektroniği", ""},
		{"Uzay Bilimleri", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.department, func(t *testing.T) {
			t.Parallel()
			if got := ytu.Faculty(tc.department); got != tc.want {
				t.Fatalf("Faculty(%q) = %q, want %q", tc.department, got, tc.want)
			}
		})
	}
}

func TestFromClaims(t *testing.T) {
	t.Parallel()
	t.Run("no university claim is not YTÜ-linked", func(t *testing.T) {
		t.Parallel()
		if got, linked := ytu.FromClaims("  ", "Bilgisayar Mühendisliği"); linked || got != (ytu.Profile{}) {
			t.Fatalf("got %+v linked=%v", got, linked)
		}
	})
	t.Run("university without department clears the department and faculty", func(t *testing.T) {
		t.Parallel()
		got, linked := ytu.FromClaims(" Yıldız Teknik Üniversitesi ", "")
		if !linked || got != (ytu.Profile{University: "Yıldız Teknik Üniversitesi"}) {
			t.Fatalf("got %+v linked=%v", got, linked)
		}
	})
	t.Run("unknown code leaves department and faculty empty", func(t *testing.T) {
		t.Parallel()
		got, linked := ytu.FromClaims("Yıldız Teknik Üniversitesi", "0Z9")
		if !linked || got.Department != "" || got.Faculty != "" {
			t.Fatalf("got %+v linked=%v", got, linked)
		}
	})
	t.Run("unknown name keeps the department and leaves faculty empty", func(t *testing.T) {
		t.Parallel()
		got, _ := ytu.FromClaims("Yıldız Teknik Üniversitesi", "Uzay Bilimleri")
		if got.Department != "Uzay Bilimleri" || got.Faculty != "" {
			t.Fatalf("got %+v", got)
		}
	})
}

// Every department in the table resolves to itself and to a faculty, so a
// typo in a name or a faculty cannot hide in the data.
func TestTableIsSelfConsistent(t *testing.T) {
	t.Parallel()
	faculties := map[string]bool{}
	for _, d := range ytu.Departments() {
		if got := ytu.Department(d); got != d {
			t.Errorf("Department(%q) = %q", d, got)
		}
		f := ytu.Faculty(d)
		if f == "" {
			t.Errorf("Faculty(%q) is empty", d)
		}
		faculties[f] = true
	}
	for f := range faculties {
		if len(f) < len(" Fakültesi") || f[len(f)-len(" Fakültesi"):] != " Fakültesi" {
			t.Errorf("faculty %q is not a faculty name", f)
		}
	}
	if len(faculties) != 11 {
		t.Errorf("the table names %d faculties, YTÜ has 11 since 2026-08-31", len(faculties))
	}
}
