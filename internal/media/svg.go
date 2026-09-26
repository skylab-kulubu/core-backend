package media

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
)

// Limits on the SVG core sanitizes: the bytes, the elements and how deep
// they nest.
const (
	maxSVGBytes    = 1 << 20
	maxSVGElements = 10_000
	maxSVGDepth    = 64
)

const (
	svgNamespace   = "http://www.w3.org/2000/svg"
	xlinkNamespace = "http://www.w3.org/1999/xlink"
	xmlNamespace   = "http://www.w3.org/XML/1998/namespace"
)

// maxSVGProlog is how far into a file isSVG reads for the root element.
const maxSVGProlog = 64 << 10

// isSVG reports whether a file is an SVG document: past an optional UTF-8
// byte order mark, its prolog (an XML declaration, comments, processing
// instructions, whitespace and at most one DOCTYPE) ends, within
// maxSVGProlog bytes, at a root element svg in the SVG namespace (or in
// none, which the sanitizer writes out in the SVG namespace). The prolog's
// declared encoding is left to the sanitizer: it is read here as bytes.
func isSVG(b []byte) bool {
	b = bytes.TrimPrefix(b[:min(len(b), maxSVGProlog)], []byte("\xEF\xBB\xBF"))
	decoder := xml.NewDecoder(bytes.NewReader(b))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	doctype := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch t := token.(type) {
		case xml.StartElement:
			return t.Name.Local == "svg" && (t.Name.Space == svgNamespace || t.Name.Space == "")
		case xml.CharData:
			if len(bytes.TrimSpace(t)) != 0 {
				return false
			}
		case xml.Directive:
			if doctype || !bytes.HasPrefix(t, []byte("DOCTYPE")) {
				return false
			}
			doctype = true
		}
	}
}

var (
	// errSVGTooLarge refuses an SVG above maxSVGBytes.
	errSVGTooLarge = errors.New("media: SVG too large to sanitize")
	// errSVGRefused refuses an SVG core does not sanitize: not XML, a
	// document type (which could declare entities), an entity, too many
	// or too deeply nested elements, or a root that is not <svg>.
	errSVGRefused = errors.New("media: SVG core does not sanitize")
)

// svgElements are the elements a sanitized SVG keeps: shapes, paths, text,
// gradients, patterns, clipping, masks, structure and descriptions. Every
// other element is removed with everything inside it: script,
// foreignObject, iframe, image, a, the animation elements (which can set
// href), filters (feImage loads images), and anything outside the SVG
// namespace (editor metadata, HTML).
var svgElements = setOf(
	"svg", "g", "defs", "symbol", "use", "title", "desc", "style",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath",
	"linearGradient", "radialGradient", "stop", "pattern", "clipPath", "mask",
)

// svgTextElements are the elements whose text a sanitized SVG keeps.
var svgTextElements = setOf("text", "tspan", "textPath", "title", "desc", "style")

// svgAttributes are the attributes a sanitized SVG keeps: geometry,
// presentation, gradients, patterns, clipping, masks and text layout. Every
// other attribute goes, among them every on* event handler.
var svgAttributes = setOf(
	"id", "class", "style", "transform", "viewBox", "preserveAspectRatio", "version", "width", "height",
	"x", "y", "x1", "y1", "x2", "y2", "cx", "cy", "r", "rx", "ry", "fx", "fy", "fr",
	"d", "points", "pathLength", "offset",
	"gradientUnits", "gradientTransform", "spreadMethod",
	"patternUnits", "patternContentUnits", "patternTransform",
	"clipPathUnits", "maskUnits", "maskContentUnits", "refX", "refY",
	"fill", "fill-opacity", "fill-rule", "stroke", "stroke-width", "stroke-linecap", "stroke-linejoin",
	"stroke-miterlimit", "stroke-dasharray", "stroke-dashoffset", "stroke-opacity", "opacity",
	"clip-path", "clip-rule", "mask", "color", "display", "visibility", "overflow",
	"stop-color", "stop-opacity", "vector-effect", "shape-rendering", "text-rendering",
	"color-interpolation", "paint-order", "isolation",
	"font-family", "font-size", "font-style", "font-weight", "font-variant", "font-stretch",
	"text-anchor", "dominant-baseline", "alignment-baseline", "baseline-shift", "letter-spacing",
	"word-spacing", "text-decoration", "writing-mode", "dx", "dy", "rotate", "textLength",
	"lengthAdjust", "startOffset", "method", "spacing", "lang",
)

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// svgNode is an element of a sanitized SVG: its kept attributes and its
// children, elements or text.
type svgNode struct {
	name     string
	attrs    []xml.Attr
	children []any // *svgNode or string
}

// sanitizeSVG reads an SVG as XML and writes it again from what it keeps:
// the elements and attributes in the allowlists above, same-document
// references (#id) and nothing that loads or runs anything. The original
// bytes are never passed through.
//
// One DOCTYPE before the root is dropped when it has no internal subset
// (Illustrator writes the SVG 1.1 one): encoding/xml fetches nothing and
// expands nothing, and it is never written out. An internal subset, an
// entity, any other directive, a second DOCTYPE, anything but whitespace,
// comments and processing instructions after the root, a root that is not
// <svg>, or more than maxSVGElements elements or nesting deeper than
// maxSVGDepth is errSVGRefused; so is a fault in the parser.
//
// An <image> stays only with a data: URI of a raster type: the bitmap is
// decoded and re-encoded like an uploaded raster image (within limit
// pixels on its longer side), and embedded again as PNG or JPEG. A bitmap
// too large to decode refuses the SVG (errImageTooLarge), and so do
// bitmaps that together cost more than the decode budget allows. An SVG
// left with nothing to draw, or larger than maxSVGBytes once its bitmaps
// are embedded again, is refused too. The caller holds the decode budget's
// SVG slot.
func sanitizeSVG(data []byte, limit int) (out []byte, err error) {
	if len(data) > maxSVGBytes {
		return nil, errSVGTooLarge
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			out, err = nil, errSVGRefused
		}
	}()
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *svgNode
	var stack []*svgNode
	elements, depth, skipped := 0, 0, 0
	doctype, closed := false, false
	var bitmapCost int64
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errSVGRefused
		}
		if closed {
			// After the root: only whitespace, comments and processing
			// instructions, which are dropped.
			switch t := token.(type) {
			case xml.Comment, xml.ProcInst:
				continue
			case xml.CharData:
				if len(bytes.TrimSpace(t)) == 0 {
					continue
				}
			}
			return nil, errSVGRefused
		}
		switch t := token.(type) {
		case xml.Directive:
			if root != nil || doctype || !bytes.HasPrefix(t, []byte("DOCTYPE")) || bytes.ContainsRune(t, '[') {
				return nil, errSVGRefused
			}
			doctype = true
		case xml.StartElement:
			elements++
			depth++
			if elements > maxSVGElements || depth > maxSVGDepth {
				return nil, errSVGRefused
			}
			if skipped > 0 {
				skipped++
				continue
			}
			inSVG := t.Name.Space == "" || t.Name.Space == svgNamespace
			var bitmap *xml.Attr
			if root == nil {
				if !inSVG || t.Name.Local != "svg" {
					return nil, errSVGRefused
				}
			} else if inSVG && t.Name.Local == "image" {
				bitmap, err = embeddedBitmap(t.Attr, limit, &bitmapCost)
				if err != nil {
					return nil, err
				}
				if bitmap == nil {
					skipped = 1
					continue
				}
			} else if !inSVG || !svgElements[t.Name.Local] {
				skipped = 1
				continue
			}
			node := &svgNode{name: t.Name.Local, attrs: sanitizeSVGAttributes(t.Attr)}
			if bitmap != nil {
				node.attrs = append(node.attrs, *bitmap)
			}
			if root == nil {
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			depth--
			if skipped > 0 {
				skipped--
				continue
			}
			stack = stack[:len(stack)-1]
			closed = len(stack) == 0
		case xml.CharData:
			if skipped > 0 || len(stack) == 0 || !svgTextElements[stack[len(stack)-1].name] {
				continue
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, string(t))
		}
	}
	if root == nil || !root.draws() {
		return nil, errSVGRefused
	}
	var written bytes.Buffer
	written.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	root.write(&written, true)
	if written.Len() > maxSVGBytes {
		return nil, errSVGTooLarge
	}
	return written.Bytes(), nil
}

// svgDrawing are the elements that draw something.
var svgDrawing = setOf("path", "rect", "circle", "ellipse", "line", "polyline", "polygon", "text", "use", "image")

// draws reports whether anything under the node draws.
func (n *svgNode) draws() bool {
	if svgDrawing[n.name] {
		return true
	}
	for _, child := range n.children {
		if c, ok := child.(*svgNode); ok && c.draws() {
			return true
		}
	}
	return false
}

// svgBitmap is a data: URI of a raster type an <image> may carry.
var svgBitmap = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp);base64,`)

// embeddedBitmap re-encodes the bitmap an <image> carries as a data: URI
// and returns it as the element's href, or nil when the element carries
// none (it is then dropped). cost adds up what decoding every bitmap of the
// SVG takes; past maxDecodedImageBytes the SVG is refused.
func embeddedBitmap(attrs []xml.Attr, limit int, cost *int64) (*xml.Attr, error) {
	var uri string
	for _, a := range attrs {
		if a.Name.Local == "href" && (a.Name.Space == "" || a.Name.Space == xlinkNamespace) {
			uri = strings.TrimSpace(a.Value)
		}
	}
	match := svgBitmap.FindString(uri)
	if match == "" {
		return nil, nil
	}
	encoded := strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, uri[len(match):])
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !isRasterType(detectContentType(raw)) {
		return nil, errSVGRefused
	}
	_, decoding, err := decodeCost(raw)
	if err != nil {
		return nil, errSVGRefused
	}
	if *cost += decoding; *cost > maxDecodedImageBytes {
		return nil, errImageTooLarge{maxPixels: MaxImagePixels}
	}
	img, err := reencodeRaster(raw, ImageHandling{Reencode: true, MaxDimension: limit})
	if err != nil {
		var tooLarge errImageTooLarge
		if errors.As(err, &tooLarge) {
			return nil, err
		}
		return nil, errSVGRefused
	}
	return &xml.Attr{Name: xml.Name{Local: "href"}, Value: "data:" + img.ctype + ";base64," + base64.StdEncoding.EncodeToString(img.body)}, nil
}

// sanitizeSVGAttributes keeps the attributes in svgAttributes whose values
// load and run nothing, href and xlink:href only to an element of the same
// document, and xml:space and xml:lang.
func sanitizeSVGAttributes(attrs []xml.Attr) []xml.Attr {
	var out []xml.Attr
	for _, a := range attrs {
		switch {
		case a.Name.Local == "href" && (a.Name.Space == "" || a.Name.Space == xlinkNamespace):
			if svgFragment.MatchString(strings.TrimSpace(a.Value)) {
				out = append(out, xml.Attr{Name: a.Name, Value: strings.TrimSpace(a.Value)})
			}
		case a.Name.Space == xmlNamespace && (a.Name.Local == "space" || a.Name.Local == "lang"):
			out = append(out, a)
		case a.Name.Space != "" || !svgAttributes[a.Name.Local]:
			// Namespace declarations are written anew; editor attributes,
			// event handlers and everything else go.
		case a.Name.Local == "style":
			if css := sanitizeCSS(a.Value, false); strings.TrimSpace(css) != "" {
				out = append(out, xml.Attr{Name: a.Name, Value: css})
			}
		case safeSVGValue(a.Name.Local, a.Value):
			out = append(out, a)
		}
	}
	return out
}

// svgFragment is a reference to an element of the same document: #id.
var svgFragment = regexp.MustCompile(`^#[A-Za-z_][A-Za-z0-9_.:-]*$`)

var (
	cssComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssFunction = regexp.MustCompile(`([a-z_-][a-z0-9_-]*)\s*\(`)
	cssString   = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	cssProperty = regexp.MustCompile(`^-{0,2}[a-z][a-z0-9-]*$`)
)

// cssFunctions are the only CSS functions a sanitized SVG keeps: colours,
// arithmetic and custom properties, and url() to an element of the same
// document. image-set(), cross-fade(), element(), src() and every other
// function go, since they can name an address in a plain string.
var cssFunctions = setOf("rgb", "rgba", "hsl", "hsla", "calc", "var", "url")

// transformFunctions are kept in transforms (the transform attributes and
// the transform property).
var transformFunctions = setOf("matrix", "translate", "translatex", "translatey", "scale", "scalex", "scaley", "rotate", "skewx", "skewy")

// transformAttributes are the attributes whose values are transforms.
var transformAttributes = setOf("transform", "gradientTransform", "patternTransform")

// safeCSSValue reports whether a CSS value or a presentation attribute
// loads and runs nothing: only the allowed functions (and, with transforms,
// the transform functions), url() only to #id, no string that looks like
// an address (a colon, a slash or a dot in it), no escapes, and no script
// or data scheme anywhere.
func safeCSSValue(value string, transforms bool) bool {
	lower := strings.ToLower(value)
	// A comment could split a word the guards below look for
	// (java/**/script:). Style has its comments removed before it gets
	// here; a presentation attribute with one is dropped.
	if strings.Contains(lower, `\`) || strings.Contains(lower, "/*") {
		return false
	}
	compact := strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, lower)
	for _, bad := range []string{"javascript:", "vbscript:", "data:", "expression", "behavior:", "-moz-binding", "@import"} {
		if strings.Contains(compact, bad) {
			return false
		}
	}
	for _, match := range cssFunction.FindAllStringSubmatchIndex(lower, -1) {
		name := lower[match[2]:match[3]]
		if !cssFunctions[name] && !(transforms && transformFunctions[name]) {
			return false
		}
		if name == "url" {
			target := strings.TrimLeft(strings.TrimSpace(lower[match[1]:]), `'"`)
			if !strings.HasPrefix(target, "#") {
				return false
			}
		}
	}
	for _, text := range cssString.FindAllString(lower, -1) {
		if strings.ContainsAny(text, ":/.") {
			return false
		}
	}
	return true
}

// safeSVGValue reports whether a presentation attribute's value loads and
// runs nothing (safeCSSValue).
func safeSVGValue(attribute, value string) bool {
	return safeCSSValue(value, transformAttributes[attribute])
}

// sanitizeDeclarations keeps the CSS declarations whose property is a
// plain name and whose value is safe (safeCSSValue).
func sanitizeDeclarations(css string) string {
	var kept []string
	for _, declaration := range strings.Split(css, ";") {
		name, value, ok := strings.Cut(declaration, ":")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok || !cssProperty.MatchString(name) || !safeCSSValue(value, name == "transform") {
			continue
		}
		kept = append(kept, strings.TrimSpace(declaration))
	}
	return strings.Join(kept, "; ")
}

// sanitizeCSS keeps what CSS may keep in a sanitized SVG. A style
// attribute (block false) keeps its safe declarations. A <style> element
// (block true) keeps its rules, each with its safe declarations, and drops
// every at-rule (@import, @font-face, @media, …) whole. CSS with escapes,
// which could spell anything another way, is removed whole, and comments
// go first.
func sanitizeCSS(css string, block bool) string {
	if strings.Contains(css, `\`) {
		return ""
	}
	css = cssComment.ReplaceAllString(css, "")
	if !block {
		return sanitizeDeclarations(css)
	}
	var out strings.Builder
	for rest := css; ; {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return out.String()
		}
		if strings.HasPrefix(rest, "@") {
			// An at-rule: to its ; or to the end of its block.
			semicolon, brace := strings.IndexByte(rest, ';'), strings.IndexByte(rest, '{')
			if brace < 0 || (semicolon >= 0 && semicolon < brace) {
				if semicolon < 0 {
					return out.String()
				}
				rest = rest[semicolon+1:]
				continue
			}
			end := closingBrace(rest, brace)
			if end < 0 {
				return out.String()
			}
			rest = rest[end+1:]
			continue
		}
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out.String()
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return out.String()
		}
		selector, body := strings.TrimSpace(rest[:open]), rest[open+1:open+end]
		rest = rest[open+end+1:]
		if strings.ContainsAny(body, "{") || strings.ContainsAny(selector, ";}") {
			continue
		}
		if declarations := sanitizeDeclarations(body); declarations != "" {
			out.WriteString(selector + " { " + declarations + " }\n")
		}
	}
}

// closingBrace is the index of the brace that closes the one at open.
func closingBrace(css string, open int) int {
	depth := 0
	for i := open; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// write serializes the node; the root declares the namespaces it needs.
func (n *svgNode) write(out *bytes.Buffer, root bool) {
	out.WriteString("<" + n.name)
	if root {
		out.WriteString(` xmlns="` + svgNamespace + `"`)
		if n.usesXlink() {
			out.WriteString(` xmlns:xlink="` + xlinkNamespace + `"`)
		}
	}
	for _, a := range n.attrs {
		name := a.Name.Local
		switch a.Name.Space {
		case xlinkNamespace:
			name = "xlink:" + name
		case xmlNamespace:
			name = "xml:" + name
		}
		out.WriteString(" " + name + `="`)
		_ = xml.EscapeText(out, []byte(a.Value))
		out.WriteString(`"`)
	}
	if len(n.children) == 0 {
		out.WriteString("/>")
		return
	}
	out.WriteString(">")
	if n.name == "style" {
		// The CSS is sanitized whole: a comment could split "@import" or
		// "url(" across two pieces of text.
		var css strings.Builder
		for _, child := range n.children {
			if text, ok := child.(string); ok {
				css.WriteString(text)
			}
		}
		_ = xml.EscapeText(out, []byte(sanitizeCSS(css.String(), true)))
		out.WriteString("</style>")
		return
	}
	for _, child := range n.children {
		switch c := child.(type) {
		case *svgNode:
			c.write(out, false)
		case string:
			_ = xml.EscapeText(out, []byte(c))
		}
	}
	out.WriteString("</" + n.name + ">")
}

func (n *svgNode) usesXlink() bool {
	for _, a := range n.attrs {
		if a.Name.Space == xlinkNamespace {
			return true
		}
	}
	for _, child := range n.children {
		if c, ok := child.(*svgNode); ok && c.usesXlink() {
			return true
		}
	}
	return false
}
