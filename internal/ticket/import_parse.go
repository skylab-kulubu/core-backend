package ticket

import (
	"encoding/csv"
	"errors"
	"io"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reading an attendee list (docs/attendee-import.md): a CSV file's text or
// a list pasted from a spreadsheet. Nothing here keeps the text: it is read
// once per request and dropped with the answer.

const (
	// MaxImportBytes is the longest list core reads, in bytes: a CSV that
	// Microsoft Forms exports with long answers stays well under it.
	MaxImportBytes = 2 << 20
	// MaxImportRows is how many rows one import takes, the header left
	// out.
	MaxImportRows = 1000
	// MaxImportName is the longest first or last name a row may carry, in
	// characters.
	MaxImportName = 100
	// maxImportEmail is the longest address (RFC 5321's path).
	maxImportEmail = 254
)

var (
	// ErrImportEmpty is a list with no row (400 import_empty).
	ErrImportEmpty = errors.New("ticket: the import list is empty")
	// ErrImportTooLarge is a list longer than MaxImportBytes (413
	// import_too_large).
	ErrImportTooLarge = errors.New("ticket: the import list is too large")
	// ErrImportTooManyRows is a list of more than MaxImportRows rows (413
	// import_too_many_rows).
	ErrImportTooManyRows = errors.New("ticket: the import list has too many rows")
	// ErrImportNoEmail is a list in which no column holds e-mail addresses
	// (400 import_no_email_column).
	ErrImportNoEmail = errors.New("ticket: no e-mail column in the import list")
	// ErrImportUnreadable is a list the CSV reader cannot read (400
	// import_unreadable).
	ErrImportUnreadable = errors.New("ticket: the import list cannot be read")
)

// ImportColumns says which of the list's columns were read as what: the
// header's text, or "1", "2"… for a list without a header. Empty for a
// column not found.
type ImportColumns struct {
	Header    bool   `json:"header"`
	Delimiter string `json:"delimiter"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	FullName  string `json:"fullName,omitempty"`
	Email     string `json:"email"`
	Session   string `json:"session,omitempty"`
}

// importLine is one row as the list gives it.
type importLine struct {
	line      int
	firstName string
	lastName  string
	email     string
	sessions  []string
}

type columnKind int

const (
	colNone columnKind = iota
	colFirst
	colLast
	colFull
	colEmail
	colSession
)

// headerNames are the column names read, folded (foldHeader): Turkish and
// English, as Microsoft Forms, Google Forms and spreadsheets name them.
var headerNames = map[string]columnKind{
	"ad": colFirst, "adi": colFirst, "adiniz": colFirst, "isim": colFirst, "isminiz": colFirst,
	"firstname": colFirst, "first": colFirst, "givenname": colFirst,
	"soyad": colLast, "soyadi": colLast, "soyadiniz": colLast, "soyisim": colLast, "soyisminiz": colLast,
	"lastname": colLast, "last": colLast, "surname": colLast, "familyname": colLast,
	"adsoyad": colFull, "adisoyadi": colFull, "adsoyadi": colFull, "adinizsoyadiniz": colFull,
	"isimsoyisim": colFull, "isimsoyad": colFull, "fullname": colFull, "name": colFull, "katilimci": colFull,
	"eposta": colEmail, "epostaadresi": colEmail, "epostaadresiniz": colEmail, "epostaniz": colEmail,
	"email": colEmail, "emailadresi": colEmail, "emailadresiniz": colEmail, "emailaddress": colEmail,
	"mail": colEmail, "mailadresi": colEmail, "mailadresiniz": colEmail,
	"oturum": colSession, "oturumlar": colSession, "oturumsecimi": colSession, "secilenoturum": colSession,
	"session": colSession, "sessions": colSession,
}

// foldHeader is a header cell compared: lower case, Turkish letters as
// their ASCII ones, only letters and digits.
func foldHeader(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.ReplaceAll(s, "İ", "i")) {
		switch r {
		case 'ı':
			r = 'i'
		case 'ş':
			r = 's'
		case 'ğ':
			r = 'g'
		case 'ü':
			r = 'u'
		case 'ö':
			r = 'o'
		case 'ç':
			r = 'c'
		case 'â':
			r = 'a'
		case 'î':
			r = 'i'
		case 'û':
			r = 'u'
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanCell is a cell without the blanks around it and the invisible
// characters spreadsheets leave (a zero-width space, a byte order mark).
func cleanCell(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff', '\u00a0':
			if r == '\u00a0' {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// detectDelimiter is the list's separator, read from its first line: a tab
// (pasted from a spreadsheet), a semicolon (Excel in Turkish) or a comma.
func detectDelimiter(text string) rune {
	first := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		first = text[:i]
	}
	tabs, semis, commas := strings.Count(first, "\t"), strings.Count(first, ";"), strings.Count(first, ",")
	switch {
	case tabs > 0 && tabs >= semis && tabs >= commas:
		return '\t'
	case semis > 0 && semis >= commas:
		return ';'
	}
	return ','
}

func delimiterName(r rune) string {
	switch r {
	case '\t':
		return "tab"
	case ';':
		return "semicolon"
	}
	return "comma"
}

// parseImport reads the list: its rows, the header left out, and which
// columns were read as what.
func parseImport(text string) ([]importLine, ImportColumns, error) {
	if len(text) > MaxImportBytes {
		return nil, ImportColumns{}, ErrImportTooLarge
	}
	if !utf8.ValidString(text) {
		return nil, ImportColumns{}, ErrImportUnreadable
	}
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	delim := detectDelimiter(strings.TrimLeft(text, "\n"))
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = false

	var records [][]string
	var lines []int
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ImportColumns{}, ErrImportUnreadable
		}
		line, _ := r.FieldPos(0)
		blank := true
		for i := range rec {
			rec[i] = cleanCell(rec[i])
			if rec[i] != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		records = append(records, rec)
		lines = append(lines, line)
		if len(records) > MaxImportRows+1 {
			return nil, ImportColumns{}, ErrImportTooManyRows
		}
	}
	if len(records) == 0 {
		return nil, ImportColumns{}, ErrImportEmpty
	}

	cols := ImportColumns{Delimiter: delimiterName(delim)}
	idx := map[columnKind]int{colFirst: -1, colLast: -1, colFull: -1, colEmail: -1, colSession: -1}
	header := records[0]
	candidates := map[columnKind][]int{}
	for i, cell := range header {
		if kind, ok := headerNames[foldHeader(cell)]; ok {
			candidates[kind] = append(candidates[kind], i)
		}
	}
	// Microsoft Forms' own respondent columns ("Email", "Name": the
	// signed-in respondent, or "anonymous") come before the form's
	// questions; a question of the form wins over them.
	for kind, at := range candidates {
		generic := func(i int) bool {
			f := foldHeader(header[i])
			return f == "email" || f == "name"
		}
		slices.SortStableFunc(at, func(a, b int) int {
			switch {
			case generic(a) && !generic(b):
				return 1
			case !generic(a) && generic(b):
				return -1
			}
			return 0
		})
		candidates[kind] = at
	}
	data := records
	if len(candidates[colEmail]) > 0 || len(candidates[colFirst]) > 0 || len(candidates[colFull]) > 0 {
		cols.Header = true
		data, lines = records[1:], lines[1:]
		for kind, at := range candidates {
			idx[kind] = at[0]
		}
		// Of two e-mail columns (Microsoft Forms' respondent column and the
		// form's own question), the one whose first row holds an address.
		if len(candidates[colEmail]) > 1 && len(data) > 0 {
			for _, at := range candidates[colEmail] {
				if at < len(data[0]) && strings.Contains(data[0][at], "@") {
					idx[colEmail] = at
					break
				}
			}
		}
	} else {
		// No header: the first column holding an address is the e-mail;
		// the others, in order, the first name, the last name and the
		// session.
		for i, cell := range header {
			if strings.Contains(cell, "@") {
				idx[colEmail] = i
				break
			}
		}
		width := 0
		for _, rec := range records {
			width = max(width, len(rec))
		}
		next := 0
		for _, kind := range []columnKind{colFirst, colLast, colSession} {
			for next < width && next == idx[colEmail] {
				next++
			}
			if next < width {
				idx[kind] = next
				next++
			}
		}
	}
	if len(data) == 0 {
		return nil, ImportColumns{}, ErrImportEmpty
	}
	if len(data) > MaxImportRows {
		return nil, ImportColumns{}, ErrImportTooManyRows
	}
	if idx[colEmail] < 0 {
		return nil, ImportColumns{}, ErrImportNoEmail
	}
	// Separate first and last name columns win over a full name.
	useFull := idx[colFull] >= 0 && (idx[colFirst] < 0 || idx[colLast] < 0)
	if !useFull {
		idx[colFull] = -1
	} else {
		idx[colFirst], idx[colLast] = -1, -1
	}
	name := func(kind columnKind) string {
		at := idx[kind]
		if at < 0 {
			return ""
		}
		if cols.Header {
			return header[at]
		}
		return strconv.Itoa(at + 1)
	}
	cols.FirstName, cols.LastName, cols.FullName = name(colFirst), name(colLast), name(colFull)
	cols.Email, cols.Session = name(colEmail), name(colSession)

	cell := func(rec []string, kind columnKind) string {
		at := idx[kind]
		if at < 0 || at >= len(rec) {
			return ""
		}
		return rec[at]
	}
	out := make([]importLine, 0, len(data))
	for i, rec := range data {
		l := importLine{line: lines[i], email: strings.ToLower(cell(rec, colEmail))}
		if useFull {
			l.firstName, l.lastName = splitFullName(cell(rec, colFull))
		} else {
			l.firstName, l.lastName = cell(rec, colFirst), cell(rec, colLast)
		}
		l.firstName, l.lastName = collapseSpaces(l.firstName), collapseSpaces(l.lastName)
		for _, part := range strings.Split(cell(rec, colSession), ";") {
			if part = cleanCell(part); part != "" {
				l.sessions = append(l.sessions, part)
			}
		}
		out = append(out, l)
	}
	return out, cols, nil
}

// splitFullName splits "Ayşe Nur Yılmaz" at its last blank: the last word is
// the last name, the rest the first.
func splitFullName(full string) (string, string) {
	full = collapseSpaces(full)
	at := strings.LastIndexByte(full, ' ')
	if at < 0 {
		return full, ""
	}
	return full[:at], full[at+1:]
}

func collapseSpaces(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// validImportEmail reports whether address can hold a Ticket: one plain
// address with a domain that has a dot, as the account e-mail rule reads it.
func validImportEmail(address string) bool {
	if address == "" || len(address) > maxImportEmail || strings.ContainsAny(address, " \t\r\n:,;<>\"") {
		return false
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	at := strings.LastIndexByte(address, '@')
	return at > 0 && strings.Contains(address[at+1:], ".") && !strings.HasSuffix(address, ".")
}
