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
	// import_unreadable). Bytes that are not UTF-8 are not: they make
	// their rows invalid (badCharacters).
	ErrImportUnreadable = errors.New("ticket: the import list cannot be read")
	// ErrImportAmbiguousEmail is a list with more than one column that may
	// be the e-mail and holds addresses ("E-posta" and "Okul e-postası"):
	// core does not guess (400 import_ambiguous_email_column). The error is
	// an *ImportAmbiguousEmailError naming the columns.
	ErrImportAmbiguousEmail = errors.New("ticket: more than one e-mail column in the import list")
)

// ImportAmbiguousEmailError names the columns that may each be the e-mail.
type ImportAmbiguousEmailError struct {
	Columns []string
}

func (e *ImportAmbiguousEmailError) Error() string {
	return ErrImportAmbiguousEmail.Error() + ": " + strings.Join(e.Columns, ", ")
}

func (e *ImportAmbiguousEmailError) Is(target error) bool { return target == ErrImportAmbiguousEmail }

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

// possessiveEndings are the Turkish second person endings a form's question
// puts on a word, folded ("Adınız", "Numaran", "Durumun"), longest first.
var possessiveEndings = []string{"iniz", "unuz", "niz", "nuz", "in", "un", "n"}

// stemAliases are words read as another: stems the endings leave that are
// not themselves a name ("İsminiz" is "isim" with its vowel dropped) and
// possessive words headerNames knows alone.
var stemAliases = map[string]string{
	"ism": "isim", "soyism": "soyisim", "isminiz": "isim", "soyisminiz": "soyisim",
	"adi": "ad", "adiniz": "ad", "soyadi": "soyad", "soyadiniz": "soyad",
}

// classifyHeader is what a header cell names. The e-mail is any header
// that says so ("E-mail adresin:", "Okul e-postası", "Your email"): its
// words hold "mail" or "eposta". The others are read word by word, the
// possessive endings off ("Adınız Soyadınız" is "Ad Soyad"). A line with
// a cell that is an address is a row, never a header (parseImport).
func classifyHeader(cell string) columnKind {
	folded := foldHeader(cell)
	if folded == "" {
		return colNone
	}
	if kind, ok := headerNames[folded]; ok {
		return kind
	}
	if strings.Contains(folded, "mail") || strings.Contains(folded, "eposta") {
		return colEmail
	}
	var stems strings.Builder
	for _, word := range strings.FieldsFunc(cell, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		w := foldHeader(word)
		if _, ok := headerNames[w]; !ok && stemAliases[w] == "" {
			for _, end := range possessiveEndings {
				if stem, cut := strings.CutSuffix(w, end); cut && len(stem) >= 2 {
					w = stem
					break
				}
			}
		}
		if alias, ok := stemAliases[w]; ok {
			w = alias
		}
		stems.WriteString(w)
	}
	return headerNames[stems.String()]
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
	// Bytes that are not UTF-8 (a CSV Excel saved in Windows-1254) become
	// U+FFFD: the rows they are in are invalid, the others are read.
	text = strings.ToValidUTF8(text, string(utf8.RuneError))
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
	// A first line with an address in it is a row: "İsmail" holds "mail",
	// but "İsmail Yılmaz,ismail@example.com" is no header.
	if !slices.ContainsFunc(header, isAddressCell) {
		for i, cell := range header {
			if kind := classifyHeader(cell); kind != colNone {
				candidates[kind] = append(candidates[kind], i)
			}
		}
	}
	// A Microsoft Forms export begins with five columns of its own: the
	// response's id, its start and completion times, and the respondent's
	// e-mail and name (blank or "anonymous" unless the form records them;
	// Microsoft's documentation gives their contents, not their labels: an
	// English export says "ID", "Start time", "Completion time", "Email",
	// "Name", a Turkish one "Kimlik", "Başlangıç saati", "Tamamlanma
	// saati", "E-posta", "Ad"). The form's own questions come after them and
	// win: the respondent's columns are read only when the form asks no
	// such question, and its name column is a full name then, never a
	// first name.
	if msFormsExport(header) {
		respEmail, respName := -1, -1
		for i := 1; i < msFormsColumns; i++ {
			switch classifyHeader(header[i]) {
			case colEmail:
				respEmail = i
			case colFirst, colLast, colFull:
				respName = i
			}
		}
		for kind, at := range candidates {
			own := slices.DeleteFunc(slices.Clone(at), func(i int) bool { return i < msFormsColumns })
			// A question that only mentions e-mail ("E-posta ile haber
			// verelim mi?") does not push out the respondent's addresses.
			if kind == colEmail && !slices.ContainsFunc(own, func(i int) bool { return holdsAddresses(records[1:], i) }) {
				own = nil
			}
			if len(own) > 0 {
				candidates[kind] = own
			} else {
				delete(candidates, kind)
			}
		}
		if len(candidates[colFirst]) == 0 && len(candidates[colLast]) == 0 && len(candidates[colFull]) == 0 && respName >= 0 {
			candidates[colFull] = []int{respName}
		}
		if len(candidates[colEmail]) == 0 && respEmail >= 0 {
			candidates[colEmail] = []int{respEmail}
		}
	}
	data := records
	if len(candidates[colEmail]) > 0 || len(candidates[colFirst]) > 0 || len(candidates[colFull]) > 0 {
		cols.Header = true
		data, lines = records[1:], lines[1:]
		for kind, at := range candidates {
			idx[kind] = at[0]
		}
		// Of several columns that may be the e-mail, the one holding
		// addresses; a column of answers ("E-posta ile haber verelim mi?")
		// holds none. Two holding addresses ("E-posta" and "Okul
		// e-postası") are refused: core names them and does not guess.
		if emails := candidates[colEmail]; len(emails) > 1 {
			holding := slices.DeleteFunc(slices.Clone(emails), func(at int) bool { return !holdsAddresses(data, at) })
			// "E-posta" and "E-posta (tekrar)" hold the same address on
			// every row: the first is read.
			if len(holding) > 1 && sameAddresses(data, holding) {
				holding = holding[:1]
			}
			if len(holding) != 1 {
				if len(holding) == 0 {
					holding = emails
				}
				names := make([]string, len(holding))
				for i, at := range holding {
					names[i] = header[at]
				}
				return nil, ImportColumns{}, &ImportAmbiguousEmailError{Columns: names}
			}
			idx[colEmail] = holding[0]
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

// holdsAddresses reports whether column at holds an address in any row.
func holdsAddresses(data [][]string, at int) bool {
	for _, rec := range data {
		if at < len(rec) && strings.Contains(rec[at], "@") {
			return true
		}
	}
	return false
}

// isAddressCell reports whether a cell is an address: an "@" and no blank
// ("Okul mailin (@std.yildiz.edu.tr)" is a question, not an address).
func isAddressCell(cell string) bool {
	return strings.Contains(cell, "@") && !strings.ContainsFunc(cell, unicode.IsSpace)
}

// sameAddresses reports whether columns hold the same address on every
// row, case ignored: a form that asks for the address twice.
func sameAddresses(data [][]string, columns []int) bool {
	at := func(rec []string, i int) string {
		if i < len(rec) {
			return strings.ToLower(rec[i])
		}
		return ""
	}
	for _, rec := range data {
		for _, i := range columns[1:] {
			if at(rec, i) != at(rec, columns[0]) {
				return false
			}
		}
	}
	return true
}

// msFormsColumns is how many columns a Microsoft Forms export starts with
// (Microsoft's "Check and share your form results": the respondent's id,
// the start and completion times, and the respondent's name and e-mail).
const msFormsColumns = 5

// msFormsExport reports whether header is a Microsoft Forms export's: its
// first column is the response's id ("ID", "Kimlik") and the next two its
// times.
func msFormsExport(header []string) bool {
	if len(header) < msFormsColumns {
		return false
	}
	switch foldHeader(header[0]) {
	case "id", "kimlik", "yanitlayankimligi":
	default:
		return false
	}
	return strings.Contains(foldHeader(header[1]), "saat") || strings.Contains(foldHeader(header[1]), "time")
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

// badCharacters reports whether s holds a character no name or address
// carries: a control character (NUL included) or U+FFFD, what a byte that
// was not UTF-8 became.
func badCharacters(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == utf8.RuneError || unicode.IsControl(r)
	})
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
