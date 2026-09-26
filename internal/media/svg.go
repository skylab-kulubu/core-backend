package media

import (
	"bytes"
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
// bytes are never passed through. A document type, an entity, a root that
// is not <svg>, or more than maxSVGElements elements nested deeper than
// maxSVGDepth is errSVGRefused.
func sanitizeSVG(data []byte) ([]byte, error) {
	if len(data) > maxSVGBytes {
		return nil, errSVGTooLarge
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *svgNode
	var stack []*svgNode
	elements, depth, skipped := 0, 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errSVGRefused
		}
		switch t := token.(type) {
		case xml.Directive:
			return nil, errSVGRefused
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
			if root == nil {
				if !inSVG || t.Name.Local != "svg" {
					return nil, errSVGRefused
				}
			} else if !inSVG || !svgElements[t.Name.Local] {
				skipped = 1
				continue
			}
			node := &svgNode{name: t.Name.Local, attrs: sanitizeSVGAttributes(t.Attr)}
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
		case xml.CharData:
			if skipped > 0 || len(stack) == 0 || !svgTextElements[stack[len(stack)-1].name] {
				continue
			}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, string(t))
		}
	}
	if root == nil {
		return nil, errSVGRefused
	}
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	root.write(&out, true)
	return out.Bytes(), nil
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
			if css := sanitizeCSS(a.Value); strings.TrimSpace(css) != "" {
				out = append(out, xml.Attr{Name: a.Name, Value: css})
			}
		case safeSVGValue(a.Value):
			out = append(out, a)
		}
	}
	return out
}

// svgFragment is a reference to an element of the same document: #id.
var svgFragment = regexp.MustCompile(`^#[A-Za-z_][A-Za-z0-9_.:-]*$`)

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssImport  = regexp.MustCompile(`(?i)@import[^;]*;?`)
	cssURL     = regexp.MustCompile(`(?i)url\(\s*(['"]?)([^'")]*)(['"]?)\s*\)`)
)

// sanitizeCSS removes from CSS what loads or runs anything: @import rules,
// url() references to anything but an element of the same document
// (replaced with none), and expression(). CSS with escapes, which could
// spell any of them another way, or that still names a script, is removed
// whole.
func sanitizeCSS(css string) string {
	if strings.Contains(css, `\`) {
		return ""
	}
	css = cssComment.ReplaceAllString(css, "")
	css = cssImport.ReplaceAllString(css, "")
	css = cssURL.ReplaceAllStringFunc(css, func(ref string) string {
		target := cssURL.FindStringSubmatch(ref)[2]
		if svgFragment.MatchString(strings.TrimSpace(target)) {
			return ref
		}
		return "none"
	})
	if !safeSVGValue(css) || strings.Contains(strings.ToLower(css), "@import") {
		return ""
	}
	return css
}

// safeSVGValue reports whether an attribute value or CSS loads and runs
// nothing: no script, data or expression URL, and every url() pointing at
// an element of the same document.
func safeSVGValue(value string) bool {
	normalized := strings.Map(func(r rune) rune {
		if r <= ' ' {
			return -1
		}
		return r
	}, strings.ToLower(value))
	for _, bad := range []string{"javascript:", "vbscript:", "data:", "expression(", "behavior:", "-moz-binding"} {
		if strings.Contains(normalized, bad) {
			return false
		}
	}
	for rest := normalized; ; {
		i := strings.Index(rest, "url(")
		if i < 0 {
			return true
		}
		rest = strings.TrimLeft(rest[i+len("url("):], `'"`)
		if !strings.HasPrefix(rest, "#") {
			return false
		}
	}
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
		_ = xml.EscapeText(out, []byte(sanitizeCSS(css.String())))
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
