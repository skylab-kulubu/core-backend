package theme

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// Guilloche draws the ticket's guilloche ground, w × h (in the units the
// page places it in, millimetres in the PDF), in colour: two families of
// fine crossing waves woven into a band pattern, and two rosettes of
// concentric waved rings. The seed (the Ticket's code) picks every period,
// phase and petal count, so a Ticket always gets the same pattern and two
// Tickets different ones. It is ornament, not a safeguard
// (docs/ticket-page.md).
func Guilloche(seed string, w, h float64, colour string) string {
	r := newDraw(seed)
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + num(w) + " " + num(h) + `" preserveAspectRatio="none">`)
	b.WriteString(`<g fill="none" stroke="` + colour + `" stroke-linejoin="round" stroke-linecap="round">`)

	// The woven waves: each family is lines strands of one wave, every
	// strand a little further along it, so neighbours cross in a regular
	// lattice; the second family runs the other way.
	lines := 30
	period := w / (2.5 + r.float())
	ripple := w / (9 + r.float()*4)
	amp := h * (0.05 + r.float()*0.015)
	small := h * (0.008 + r.float()*0.006)
	for family := 0; family < 2; family++ {
		phase := r.float() * 2 * math.Pi
		drift := 2 * math.Pi / float64(lines) * 3
		if family == 1 {
			drift = -drift
		}
		b.WriteString(`<path stroke-width="` + num3(h*0.0011) + `" stroke-opacity="0.3" d="`)
		for i := 0; i < lines; i++ {
			y0 := h * (float64(i) + 0.5) / float64(lines)
			steps := 72
			for s := 0; s <= steps; s++ {
				x := w * float64(s) / float64(steps)
				y := y0 + amp*math.Sin(2*math.Pi*x/period+phase+float64(i)*drift) +
					small*math.Sin(2*math.Pi*x/ripple+phase*2)
				b.WriteString(move(s == 0) + num(x) + " " + num(y))
			}
		}
		b.WriteString(`"/>`)
	}

	// Two rosettes: concentric rings, each waved with the same petal count
	// and turned a little from the last, which draws the classic
	// interlaced rose.
	for _, at := range [][2]float64{{0.27 + r.float()*0.06, 0.62 + r.float()*0.08}, {0.86 + r.float()*0.04, 0.30 + r.float()*0.1}} {
		cx, cy := w*at[0], h*at[1]
		petals := 18 + int(r.float()*14)
		inner := h * 0.08
		rings := 10
		gap := h * 0.016
		wave := h * (0.012 + r.float()*0.006)
		twist := (0.35 + r.float()*0.3) * 2 * math.Pi / float64(petals)
		b.WriteString(`<path stroke-width="` + num3(h*0.001) + `" stroke-opacity="0.3" d="`)
		for ring := 0; ring < rings; ring++ {
			radius := inner + gap*float64(ring)
			steps := petals * 8
			for s := 0; s <= steps; s++ {
				a := 2 * math.Pi * float64(s) / float64(steps)
				rr := radius + wave*math.Sin(float64(petals)*a+twist*float64(ring))
				b.WriteString(move(s == 0) + num(cx+rr*math.Cos(a)) + " " + num(cy+rr*math.Sin(a)))
			}
		}
		b.WriteString(`"/>`)
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}

func move(first bool) string {
	if first {
		return "M"
	}
	return "L"
}

// draw is a deterministic stream of numbers in [0, 1) from a seed:
// SHA-256 in counter mode, not for secrets.
type draw struct {
	seed [32]byte
	n    uint64
	buf  []byte
}

func newDraw(seed string) *draw {
	return &draw{seed: sha256.Sum256([]byte("ticket guilloche v1\x00" + seed))}
}

func (d *draw) float() float64 {
	if len(d.buf) < 8 {
		var block [40]byte
		copy(block[:], d.seed[:])
		binary.BigEndian.PutUint64(block[32:], d.n)
		d.n++
		sum := sha256.Sum256(block[:])
		d.buf = append(d.buf, sum[:]...)
	}
	v := binary.BigEndian.Uint64(d.buf[:8])
	d.buf = d.buf[8:]
	return float64(v>>11) / float64(uint64(1)<<53)
}

func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

func num3(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}
