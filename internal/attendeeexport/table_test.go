package attendeeexport

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func TestCSVTextNeverReadsAsAFormula(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"=1+1": "'=1+1", "+90": "'+90", "-5": "'-5", "@SUM(A1)": "'@SUM(A1)", "\tx": "'\tx", "\rx": "'\rx",
		"Ada": "Ada", "": "", "a=b": "a=b",
	} {
		if got := csvText(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestCSVShape(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 27, 6, 30, 0, 0, time.UTC)
	out, err := CSV(Table{
		Columns: []Column{{Header: "Ad"}, {Header: "Sayı", Kind: Number}, {Header: "Zaman", Kind: DateTime}, {Header: "Boş", Kind: DateTime}},
		Rows:    [][]any{{"Çağrı; \"Ş\"", 3, &at, (*time.Time)(nil)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := BOM + "Ad;Sayı;Zaman;Boş\r\n\"Çağrı; \"\"Ş\"\"\";3;27.11.2026 09:30;\r\n"
	if string(out) != want {
		t.Fatalf("%q", out)
	}
}

func TestXLSXCellsAndSheet(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 27, 6, 30, 0, 0, time.UTC)
	out, err := XLSX(Table{
		Sheet:   "Ad/soyad [liste] çok uzun bir sayfa adı olsun",
		Columns: []Column{{Header: "Ad"}, {Header: "Sayı", Kind: Number}, {Header: "Zaman", Kind: DateTime}},
		Rows:    [][]any{{"=1+1", 7, &at}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := excelize.OpenReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	sheet := wb.GetSheetName(0)
	if sheet != "Ad-soyad -liste- çok uzun bir s" {
		t.Fatalf("sheet %q", sheet)
	}
	if f, _ := wb.GetCellFormula(sheet, "A2"); f != "" {
		t.Fatalf("a formula: %q", f)
	}
	if v, _ := wb.GetCellValue(sheet, "A2"); v != "=1+1" {
		t.Fatalf("text %q", v)
	}
	if typ, _ := wb.GetCellType(sheet, "B2"); typ != excelize.CellTypeUnset && typ != excelize.CellTypeNumber {
		t.Fatalf("number type %v", typ)
	}
	// Istanbul's wall clock, shown as dd.mm.yyyy hh:mm.
	if v, _ := wb.GetCellValue(sheet, "C2"); v != "27.11.2026 09:30" {
		t.Fatalf("time %q", v)
	}
	raw, _ := wb.GetCellValue(sheet, "C2", excelize.Options{RawCellValue: true})
	if !strings.HasPrefix(raw, "46353.395833") {
		t.Fatalf("serial %q", raw)
	}
}

func TestFileName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 21, 5, 0, 0, time.UTC)
	if got := FileName("ARTLAB · Yıldız Çağı & İş Günü", "matrix", "xlsx", now); got != "artlab-yildiz-cagi-is-gunu-katilim-matrisi-20261127-0005.xlsx" {
		t.Fatalf("%q", got)
	}
	if got := FileName("!!!", "attendees", "csv", now); got != "etkinlik-katilimcilar-20261127-0005.csv" {
		t.Fatalf("%q", got)
	}
	if got := Disposition("a b.csv"); got != `attachment; filename="a b.csv"` {
		t.Fatalf("%q", got)
	}
}

// A large Event: 3 000 rows of 20 columns in both formats, well within a
// request's time.
func TestLargeTable(t *testing.T) {
	t.Parallel()
	cols := make([]Column, 20)
	for i := range cols {
		cols[i] = Column{Header: fmt.Sprintf("Sütun %d", i)}
	}
	cols[19].Kind = DateTime
	at := time.Now()
	rows := make([][]any, 3000)
	for i := range rows {
		row := make([]any, 20)
		for j := range 19 {
			row[j] = fmt.Sprintf("hücre %d-%d", i, j)
		}
		row[19] = &at
		rows[i] = row
	}
	start := time.Now()
	x, err := XLSX(Table{Sheet: "Büyük", Columns: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CSV(Table{Columns: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	t.Logf("xlsx %d bytes, csv %d bytes, %s", len(x), len(c), time.Since(start))
	wb, err := excelize.OpenReader(bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	got, _ := wb.GetRows("Büyük")
	if len(got) != 3001 {
		t.Fatalf("%d rows", len(got))
	}
}
