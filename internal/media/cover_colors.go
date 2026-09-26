package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"sort"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	coverColorClusters    = 12
	coverColorIterations  = 12
	coverColorMinDistance = 40.0
	coverColorMaxPixels   = 24_000_000
)

type colorPoint struct {
	r      float64
	g      float64
	b      float64
	weight float64
}

type colorCluster struct {
	r     float64
	g     float64
	b     float64
	share float64
}

// coverColorDecodes bounds how many images are decoded for cover colours at
// once, so concurrent uploads can't stack full-size decodes in memory.
var coverColorDecodes = make(chan struct{}, 2)

func ExtractCoverColors(data []byte) []string {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > coverColorMaxPixels {
		return []string{}
	}
	// A WebP's DecodeConfig reports its VP8X canvas, but the decoder allocates
	// the frame the bitstream declares, which can be far larger. Cover colours
	// are decoration, so WebP images go without them rather than trust the
	// canvas.
	if format == "webp" {
		return []string{}
	}
	coverColorDecodes <- struct{}{}
	img, _, err := image.Decode(bytes.NewReader(data))
	<-coverColorDecodes
	if err != nil {
		return []string{}
	}
	img = resizeCoverImage(img)
	points := coverHistogram(img)
	if len(points) == 0 {
		return []string{}
	}
	clusters := coverKMeans(points)
	if len(clusters) == 0 {
		return []string{}
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		return clusters[i].score() > clusters[j].score()
	})
	colorful := make([]colorCluster, 0, len(clusters))
	for _, cluster := range clusters {
		if !cluster.extreme() {
			colorful = append(colorful, cluster)
		}
	}
	candidates := clusters
	if len(colorful) >= 2 {
		candidates = colorful
	}
	picked := make([]color.NRGBA, 0, 5)
	for _, cluster := range candidates {
		if len(picked) == 5 {
			break
		}
		candidate := backdropColor(cluster.color())
		farEnough := true
		for _, existing := range picked {
			if colorDistance(existing, candidate) < coverColorMinDistance {
				farEnough = false
				break
			}
		}
		if farEnough {
			picked = append(picked, candidate)
		}
	}
	out := make([]string, 0, len(picked))
	for _, c := range picked {
		out = append(out, fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B))
	}
	return out
}

func resizeCoverImage(src image.Image) image.Image {
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return src
	}
	if bounds.Dx() <= 120 {
		return src
	}
	height := int(math.Round(float64(bounds.Dy()) * 120 / float64(bounds.Dx())))
	if height < 1 {
		height = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, 120, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Src, nil)
	return dst
}

func coverHistogram(img image.Image) []colorPoint {
	type sums struct {
		r      float64
		g      float64
		b      float64
		weight float64
	}
	bounds := img.Bounds()
	byKey := make(map[int]*sums)
	order := make([]int, 0)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if c.A < 128 {
				continue
			}
			key := int(c.R>>3)<<10 | int(c.G>>3)<<5 | int(c.B>>3)
			p, ok := byKey[key]
			if !ok {
				p = &sums{}
				byKey[key] = p
				order = append(order, key)
			}
			p.r += float64(c.R)
			p.g += float64(c.G)
			p.b += float64(c.B)
			p.weight++
		}
	}
	out := make([]colorPoint, 0, len(order))
	for _, key := range order {
		p := byKey[key]
		out = append(out, colorPoint{r: p.r / p.weight, g: p.g / p.weight, b: p.b / p.weight, weight: p.weight})
	}
	return out
}

func coverKMeans(points []colorPoint) []colorCluster {
	centers := initialCoverCenters(points)
	assignment := make([]int, len(points))
	for iteration := 0; iteration < coverColorIterations; iteration++ {
		next := make([]int, len(points))
		for i, point := range points {
			next[i] = nearestCoverCenter(point, centers)
		}
		sums := make([]colorPoint, len(centers))
		for i, point := range points {
			s := &sums[next[i]]
			s.r += point.r * point.weight
			s.g += point.g * point.weight
			s.b += point.b * point.weight
			s.weight += point.weight
		}
		for i, sum := range sums {
			if sum.weight == 0 {
				continue
			}
			centers[i] = colorPoint{r: sum.r / sum.weight, g: sum.g / sum.weight, b: sum.b / sum.weight}
		}
		converged := sameCoverAssignment(assignment, next)
		assignment = next
		if converged && iteration > 0 {
			break
		}
	}
	totals := make([]float64, len(centers))
	all := 0.0
	for i, point := range points {
		totals[assignment[i]] += point.weight
		all += point.weight
	}
	out := make([]colorCluster, 0, len(centers))
	for i, center := range centers {
		if totals[i] == 0 {
			continue
		}
		out = append(out, colorCluster{r: center.r, g: center.g, b: center.b, share: totals[i] / all})
	}
	return out
}

func initialCoverCenters(points []colorPoint) []colorPoint {
	sorted := append([]colorPoint(nil), points...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].weight > sorted[j].weight
	})
	centers := []colorPoint{sorted[0]}
	for len(centers) < coverColorClusters && len(centers) < len(sorted) {
		best := colorPoint{}
		bestScore := -1.0
		for _, point := range sorted {
			distance := math.Inf(1)
			for _, center := range centers {
				distance = math.Min(distance, point.distance(center))
			}
			score := distance * distance * math.Sqrt(point.weight)
			if score > bestScore {
				bestScore = score
				best = point
			}
		}
		if bestScore <= 0 {
			break
		}
		centers = append(centers, best)
	}
	for i := range centers {
		centers[i].weight = 0
	}
	return centers
}

func nearestCoverCenter(point colorPoint, centers []colorPoint) int {
	index := 0
	best := math.Inf(1)
	for i, center := range centers {
		if distance := point.distance(center); distance < best {
			best = distance
			index = i
		}
	}
	return index
}

func sameCoverAssignment(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p colorPoint) distance(other colorPoint) float64 {
	dr := p.r - other.r
	dg := p.g - other.g
	db := p.b - other.b
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func (c colorCluster) hsl() (float64, float64, float64) {
	r := c.r / 255
	g := c.g / 255
	b := c.b / 255
	maximum := math.Max(r, math.Max(g, b))
	minimum := math.Min(r, math.Min(g, b))
	lightness := (maximum + minimum) / 2
	if maximum == minimum {
		return 0, 0, lightness
	}
	delta := maximum - minimum
	saturation := delta / (maximum + minimum)
	if lightness > 0.5 {
		saturation = delta / (2 - maximum - minimum)
	}
	hue := 0.0
	switch maximum {
	case r:
		hue = math.Mod((g-b)/delta, 6)
	case g:
		hue = (b-r)/delta + 2
	default:
		hue = (r-g)/delta + 4
	}
	hue *= 60
	if hue < 0 {
		hue += 360
	}
	return hue, saturation, lightness
}

func (c colorCluster) extreme() bool {
	_, _, lightness := c.hsl()
	return lightness < 0.12 || lightness > 0.85
}

func (c colorCluster) score() float64 {
	_, saturation, _ := c.hsl()
	score := c.share * (0.1 + 1.5*saturation)
	if c.extreme() {
		score *= 0.15
	}
	return score
}

func (c colorCluster) color() color.NRGBA {
	return color.NRGBA{R: byte(math.Round(c.r)), G: byte(math.Round(c.g)), B: byte(math.Round(c.b)), A: 255}
}

func backdropColor(c color.NRGBA) color.NRGBA {
	cluster := colorCluster{r: float64(c.R), g: float64(c.G), b: float64(c.B)}
	hue, saturation, lightness := cluster.hsl()
	if saturation < 0.08 {
		return hslColor(hue, saturation, clamp(lightness, 0.3, 0.55))
	}
	return hslColor(hue, clamp(saturation, 0.45, 0.9), clamp(lightness, 0.36, 0.56))
}

func hslColor(hue, saturation, lightness float64) color.NRGBA {
	chroma := (1 - math.Abs(2*lightness-1)) * saturation
	x := chroma * (1 - math.Abs(math.Mod(hue/60, 2)-1))
	m := lightness - chroma/2
	r, g, b := 0.0, 0.0, 0.0
	switch {
	case hue < 60:
		r, g = chroma, x
	case hue < 120:
		r, g = x, chroma
	case hue < 180:
		g, b = chroma, x
	case hue < 240:
		g, b = x, chroma
	case hue < 300:
		r, b = x, chroma
	default:
		r, b = chroma, x
	}
	return color.NRGBA{
		R: byte(math.Round((r + m) * 255)),
		G: byte(math.Round((g + m) * 255)),
		B: byte(math.Round((b + m) * 255)),
		A: 255,
	}
}

func colorDistance(a, b color.NRGBA) float64 {
	dr := float64(a.R) - float64(b.R)
	dg := float64(a.G) - float64(b.G)
	db := float64(a.B) - float64(b.B)
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func clamp(value, minimum, maximum float64) float64 {
	return math.Min(math.Max(value, minimum), maximum)
}
