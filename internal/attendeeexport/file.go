package attendeeexport

import (
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// File is a made export: its bytes, media type and name.
type File struct {
	Body        []byte
	ContentType string
	Name        string
	Rows        int
}

// Media types of the two formats.
const (
	XLSXType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	CSVType  = "text/csv; charset=utf-8"
)

// Make writes t in d's format, named after the Event, the report and now.
func Make(d *ticket.ExportData, t Table, now time.Time) (File, error) {
	out := File{Rows: len(t.Rows), Name: FileName(d.Event.Name, d.Request.Report, d.Request.Format, now)}
	var err error
	switch d.Request.Format {
	case "xlsx":
		out.Body, err = XLSX(t)
		out.ContentType = XLSXType
	case "csv":
		out.Body, err = CSV(t)
		out.ContentType = CSVType
	default:
		err = fmt.Errorf("attendeeexport: unknown format %q", d.Request.Format)
	}
	return out, err
}

// FileName is "<event>-<report>-<YYYYMMDD-HHMM>.<format>": the Event's
// name in lower-case ASCII (Turkish letters folded), Istanbul's clock.
func FileName(eventName, report, format string, now time.Time) string {
	slug := Slug(eventName)
	if slug == "" {
		slug = "etkinlik"
	}
	return fmt.Sprintf("%s-%s-%s.%s", slug, fileSlugs[report], now.In(istanbul).Format("20060102-1504"), format)
}

var fold = strings.NewReplacer(
	"ç", "c", "Ç", "c", "ğ", "g", "Ğ", "g", "ı", "i", "I", "i", "İ", "i",
	"ö", "o", "Ö", "o", "ş", "s", "Ş", "s", "ü", "u", "Ü", "u", "â", "a", "î", "i", "û", "u",
)

// Slug is name in lower-case ASCII letters and digits joined by dashes,
// at most 48 characters.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range fold.Replace(name) {
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}

// Disposition is the Content-Disposition of an attachment named name.
func Disposition(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}
