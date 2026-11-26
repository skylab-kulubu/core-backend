package certificate

import (
	"encoding/json"
	"errors"
	"regexp"
)

// PDFText is what pdf-tools' /v1/inspect says of a PDF's first page and
// its text (pdf-tools/README.md): the spans (one per text-showing
// operator), and the runs it groups them into (one style along a line).
// Coordinates are pt on the visible page, origin top left, y down.
type PDFText struct {
	Info PDFInfo
	// Truncated: the PDF has more spans than pdf-tools reads (5000); none
	// of them can be removed then.
	Truncated bool
	Spans     []PDFSpan
	Runs      []PDFRun
	// Objects counts what the page draws; nil from a pdf-tools that does
	// not count them.
	Objects *PDFObjects
}

// PDFSpan is one text object of the PDF.
type PDFSpan struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Quad is the text's box turned with it: bottom left, bottom right,
	// top right, top left as the text reads.
	Quad   [][2]float64 `json:"quad"`
	Origin struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"origin"`
	// Angle is degrees, clockwise as shown (CSS rotate); Size the font
	// size as shown, pt.
	Angle       float64 `json:"angle"`
	Size        float64 `json:"size"`
	Font        PDFFont `json:"font"`
	Color       string  `json:"color"`
	Opacity     float64 `json:"opacity"`
	RenderMode  int     `json:"renderMode"`
	Line        int     `json:"line"`
	Block       int     `json:"block"`
	Run         int     `json:"run"`
	Convertible bool    `json:"convertible"`
	Reason      string  `json:"reason"`
}

// PDFFont is a span's font as the PDF names and describes it.
type PDFFont struct {
	Name   string `json:"name"`
	Family string `json:"family"`
	Weight int    `json:"weight"`
	Italic bool   `json:"italic"`
	Serif  bool   `json:"serif"`
	Mono   bool   `json:"mono"`
	Script bool   `json:"script"`
	// Ascent and Descent are per 1 pt of size (Descent below 0).
	Ascent  float64 `json:"ascent"`
	Descent float64 `json:"descent"`
}

// PDFRun is one style along a line: what a reader takes for one piece of
// text. Its id in core's answers is its first span's.
type PDFRun struct {
	SpanIDs []string `json:"spanIds"`
	Text    string   `json:"text"`
	Line    int      `json:"line"`
	Block   int      `json:"block"`
}

// ID is the run's id: its first span's, which no other run has.
func (r PDFRun) ID() string { return r.SpanIDs[0] }

// PDFObjects counts the objects a page draws, Form XObjects' included.
type PDFObjects struct {
	Text    int `json:"text"`
	Path    int `json:"path"`
	Image   int `json:"image"`
	Shading int `json:"shading"`
	Form    int `json:"form"`
}

// maxPDFSpans is the most spans pdf-tools reads of a page.
const maxPDFSpans = 5000

var spanIDPattern = regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,9}){0,32}$`)

// parsePDFText reads /v1/inspect's answer, which core trusts no more than
// the PDF: a malformed one is pdf_tools_unavailable.
func parsePDFText(body []byte) (PDFText, error) {
	var answer struct {
		Pages int `json:"pages"`
		Page  struct {
			WidthPt  float64 `json:"widthPt"`
			HeightPt float64 `json:"heightPt"`
			Rotate   int     `json:"rotate"`
		} `json:"page"`
		Encrypted bool        `json:"encrypted"`
		TextCount int         `json:"textCount"`
		Truncated bool        `json:"truncated"`
		Objects   *PDFObjects `json:"objects"`
		Spans     []PDFSpan   `json:"spans"`
		Runs      []PDFRun    `json:"runs"`
	}
	bad := pdfToolsUnavailable(errors.New("pdf-tools: inspect answered no text"))
	if err := json.Unmarshal(body, &answer); err != nil || answer.Pages < 1 || !finite(answer.Page.WidthPt, answer.Page.HeightPt) ||
		answer.Page.WidthPt <= 0 || answer.Page.HeightPt <= 0 || len(answer.Spans) > maxPDFSpans || len(answer.Runs) > maxPDFSpans {
		return PDFText{}, bad
	}
	ids := make(map[string]bool, len(answer.Spans))
	for _, span := range answer.Spans {
		if !spanIDPattern.MatchString(span.ID) || ids[span.ID] || len(span.Quad) != 4 ||
			!finite(span.Origin.X, span.Origin.Y, span.Angle, span.Size, span.Opacity, span.Font.Ascent, span.Font.Descent) {
			return PDFText{}, bad
		}
		for _, point := range span.Quad {
			if !finite(point[0], point[1]) {
				return PDFText{}, bad
			}
		}
		ids[span.ID] = true
	}
	inRun := make(map[string]bool, len(answer.Spans))
	for _, run := range answer.Runs {
		if len(run.SpanIDs) == 0 {
			return PDFText{}, bad
		}
		for _, id := range run.SpanIDs {
			if !ids[id] || inRun[id] {
				return PDFText{}, bad
			}
			inRun[id] = true
		}
	}
	return PDFText{
		Info: PDFInfo{
			Pages: answer.Pages, WidthPt: answer.Page.WidthPt, HeightPt: answer.Page.HeightPt, Rotate: answer.Page.Rotate,
			Encrypted: answer.Encrypted, TextCount: answer.TextCount,
		},
		Truncated: answer.Truncated, Spans: answer.Spans, Runs: answer.Runs, Objects: answer.Objects,
	}, nil
}
