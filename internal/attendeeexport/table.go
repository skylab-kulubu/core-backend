// Package attendeeexport makes the files of an attendee export
// (docs/attendee-export.md): a report of an Event's attendees as an Excel
// workbook (XLSX) or a CSV file. The ticket service decides who may export
// and reads the data (ticket.Service.BeginExport); this package lays it
// out as a table and writes the file.
//
// Every date and time is Istanbul's wall clock. The XLSX has Turkish
// column headers, a frozen header row and an auto filter; its cells are
// values, never formulas. The CSV is UTF-8 with a byte order mark and
// semicolons, as Excel opens it on a Turkish system, and a cell that a
// spreadsheet would read as a formula is written as text (OWASP CSV
// injection).
package attendeeexport

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Istanbul's clock whatever the image carries.

	"github.com/xuri/excelize/v2"
)

// Kind is how a column's cells are written.
type Kind int

const (
	Text Kind = iota
	// Number cells are ints.
	Number
	// DateTime cells are *time.Time (nil is empty), written as Istanbul's
	// date and minute.
	DateTime
)

// Column is a table's column.
type Column struct {
	Header string
	Kind   Kind
	// Width is the XLSX column width in characters; 0 is 14.
	Width float64
}

// Table is one report: a sheet of a workbook, or a CSV file.
type Table struct {
	// Sheet is the workbook's sheet name (at most 31 characters).
	Sheet   string
	Columns []Column
	// Rows hold a cell per column: string for Text, int for Number,
	// *time.Time for DateTime.
	Rows [][]any
}

var istanbul = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return loc
	}
	return time.FixedZone("Europe/Istanbul", 3*60*60)
}()

// local is t as Istanbul's wall clock, in a location-less (UTC) time: a
// spreadsheet's date has no zone, so the wall clock is what it keeps.
func local(t time.Time) time.Time {
	l := t.In(istanbul)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
}

// FormatTime is t as the CSV and the lists write it: 26.11.2026 14:05.
func FormatTime(t time.Time) string {
	return t.In(istanbul).Format("02.01.2006 15:04")
}

// XLSX writes the table as a workbook of one sheet.
func XLSX(t Table) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	sheet := sheetName(t.Sheet)
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return nil, err
	}
	header, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E8EEF7"}},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true},
	})
	if err != nil {
		return nil, err
	}
	when, err := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("dd.mm.yyyy hh:mm")})
	if err != nil {
		return nil, err
	}
	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return nil, err
	}
	for i, col := range t.Columns {
		width := col.Width
		if width == 0 {
			width = 14
		}
		if err := sw.SetColWidth(i+1, i+1, width); err != nil {
			return nil, err
		}
	}
	if err := sw.SetPanes(&excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}); err != nil {
		return nil, err
	}
	head := make([]any, len(t.Columns))
	for i, col := range t.Columns {
		head[i] = excelize.Cell{StyleID: header, Value: col.Header}
	}
	if err := sw.SetRow("A1", head); err != nil {
		return nil, err
	}
	for r, row := range t.Rows {
		cells := make([]any, len(t.Columns))
		for i, col := range t.Columns {
			var v any
			if i < len(row) {
				v = row[i]
			}
			cells[i] = xlsxCell(col.Kind, v, when)
		}
		cell, _ := excelize.CoordinatesToCellName(1, r+2)
		if err := sw.SetRow(cell, cells); err != nil {
			return nil, err
		}
	}
	if err := sw.Flush(); err != nil {
		return nil, err
	}
	if len(t.Columns) > 0 {
		last, _ := excelize.CoordinatesToCellName(len(t.Columns), len(t.Rows)+1)
		if err := f.AutoFilter(sheet, "A1:"+last, nil); err != nil {
			return nil, err
		}
	}
	if err := f.SetDocProps(&excelize.DocProperties{Creator: "SKY LAB", Title: t.Sheet, Language: "tr-TR"}); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := f.Write(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func xlsxCell(kind Kind, v any, when int) any {
	switch kind {
	case Number:
		if n, ok := v.(int); ok {
			return n
		}
		return nil
	case DateTime:
		if t, ok := v.(*time.Time); ok && t != nil {
			return excelize.Cell{StyleID: when, Value: local(*t)}
		}
		return nil
	default:
		s, _ := v.(string)
		// A string cell is a string: excelize writes it as one, never a
		// formula, whatever it starts with.
		return s
	}
}

// BOM is the UTF-8 byte order mark that makes Excel read a CSV as UTF-8.
const BOM = "\xef\xbb\xbf"

// CSV writes the table as a CSV file: BOM, semicolons, CRLF.
func CSV(t Table) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(BOM)
	w := csv.NewWriter(&out)
	w.Comma = ';'
	w.UseCRLF = true
	head := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		head[i] = csvText(col.Header)
	}
	if err := w.Write(head); err != nil {
		return nil, err
	}
	record := make([]string, len(t.Columns))
	for _, row := range t.Rows {
		for i, col := range t.Columns {
			var v any
			if i < len(row) {
				v = row[i]
			}
			record[i] = csvCell(col.Kind, v)
		}
		if err := w.Write(record); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return out.Bytes(), w.Error()
}

func csvCell(kind Kind, v any) string {
	switch kind {
	case Number:
		if n, ok := v.(int); ok {
			return fmt.Sprint(n)
		}
		return ""
	case DateTime:
		if t, ok := v.(*time.Time); ok && t != nil {
			return FormatTime(*t)
		}
		return ""
	default:
		s, _ := v.(string)
		return csvText(s)
	}
}

// csvText keeps a spreadsheet from reading s as a formula: a cell that
// starts with = + - @ or a tab or carriage return gets a leading
// apostrophe (OWASP's CSV injection advice).
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// sheetName is name as Excel takes it: at most 31 characters, none of
// []:*?/\.
func sheetName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '-'
		}
		return r
	}, name)
	if runes := []rune(name); len(runes) > 31 {
		name = string(runes[:31])
	}
	if strings.TrimSpace(name) == "" {
		return "Liste"
	}
	return name
}

func ptr[T any](v T) *T { return &v }
