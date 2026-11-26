package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
)

var regularFont = parseFont(goregular.TTF)
var boldFont = parseFont(gobold.TTF)

func parseFont(data []byte) *truetype.Font {
	f, err := truetype.Parse(data)
	if err != nil {
		panic(err)
	} // Embedded Go font.
	return f
}

type point struct{ x, y float64 }
type plotLine struct {
	name   string
	points []point
}

var palette = []string{"#67DED3", "#FFBA71", "#A89BFF", "#76BCFF", "#F99CB9", "#BADD82"}

func seriesLabel(metric map[string]string) string {
	for _, key := range []string{"pod", "deployment", "application", "app", "instance", "job", "__name__"} {
		if name := metric[key]; name != "" {
			return name
		}
	}
	keys := make([]string, 0, len(metric))
	for k := range metric {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+metric[k])
	}
	return strings.Join(parts, ", ")
}

func fit(dc *gg.Context, s string, w float64) string {
	s = strings.Join(strings.Fields(s), " ")
	if width, _ := dc.MeasureString(s); width <= w {
		return s
	}
	runes := []rune(s)
	n := sort.Search(len(runes), func(i int) bool { width, _ := dc.MeasureString(string(runes[:i+1]) + "…"); return width > w })
	return string(runes[:n]) + "…"
}

func axisNumber(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

func (g graphData) png(title string) ([]byte, error) {
	xmin, xmax := math.Inf(1), math.Inf(-1)
	ymin, ymax := math.Inf(1), math.Inf(-1)
	var lines []plotLine
	for _, series := range g.Data.Series {
		if len(series.Values) != len(series.Timestamps) {
			return nil, errors.New("unaligned graph series")
		}
		line := plotLine{name: seriesLabel(series.Metric)}
		valid := false
		for i, raw := range series.Values {
			x := series.Timestamps[i]
			y, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(y) || math.IsInf(y, 0) {
				line.points = append(line.points, point{x, math.NaN()})
				continue
			}
			valid = true
			line.points = append(line.points, point{x, y})
			xmin, xmax = math.Min(xmin, x), math.Max(xmax, x)
			ymin, ymax = math.Min(ymin, y), math.Max(ymax, y)
		}
		if valid {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, nil
	}
	if xmin == xmax {
		xmin -= 30
		xmax += 30
	}
	if ymin == ymax {
		pad := math.Abs(ymin) * 0.1
		if pad == 0 {
			ymax = 1
		} else {
			ymin -= pad
			ymax += pad
		}
	}
	// Round the vertical scale to a readable multiple of 1, 2, 5 or 10.
	rawStep := (ymax - ymin) / 4
	magnitude := math.Pow(10, math.Floor(math.Log10(rawStep)))
	step := magnitude
	for _, m := range []float64{1, 2, 5, 10} {
		step = m * magnitude
		if step >= rawStep {
			break
		}
	}
	lo, hi := math.Floor(ymin/step)*step, math.Ceil(ymax/step)*step
	if !(hi > lo) || math.IsInf(hi-lo, 0) {
		return nil, errors.New("graph range exceeds representable scale")
	}

	const width, height = 1120, 480
	const left, right, top, bottom = 116., 1062., 117., 365.
	dc := gg.NewContext(width, height)
	dc.SetHexColor("#161D29")
	dc.Clear()
	setFont := func(f *truetype.Font, size float64) {
		dc.SetFontFace(truetype.NewFace(f, &truetype.Options{Size: size}))
	}
	setFont(boldFont, 34)
	dc.SetHexColor("#F4F7FB")
	if g.Name != "" && g.Name != g.Metadata.Query {
		title = g.Name
	}
	dc.DrawString(fit(dc, title, width-88), 42, 54)
	setFont(regularFont, 20)
	asTime := func(x float64) time.Time {
		sec, frac := math.Modf(x)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC()
	}
	start, end := asTime(xmin), asTime(xmax)
	stamp := "02 Jan 2006 15:04"
	endFormat := "15:04"
	tickFormat := "15:04"
	if start.YearDay() != end.YearDay() || start.Year() != end.Year() {
		endFormat = stamp
		tickFormat = "02 Jan 15:04"
	}
	if xmax-xmin < 60 {
		stamp += ":05"
		endFormat += ":05"
		tickFormat += ":05"
	}
	dc.SetHexColor("#B5C4D7")
	dc.DrawString(start.Format(stamp)+"  –  "+end.Format(endFormat)+" UTC", 43, 88)
	px := func(x float64) float64 { return left + (x-xmin)/(xmax-xmin)*(right-left) }
	py := func(y float64) float64 { return bottom - (y-lo)/(hi-lo)*(bottom-top) }
	setFont(regularFont, 20)
	for i := 0; i <= int(math.Round((hi-lo)/step)); i++ {
		y := lo + float64(i)*step
		dc.SetHexColor("#374558")
		dc.SetLineWidth(1)
		dc.DrawLine(left, py(y), right, py(y))
		dc.Stroke()
		dc.SetHexColor("#CAD4E1")
		dc.DrawStringAnchored(axisNumber(y), left-14, py(y)-1, 1, .5)
	}
	for i := 0; i <= 4; i++ {
		fraction := float64(i) / 4
		x := xmin + fraction*(xmax-xmin)
		dc.SetHexColor("#B5C4D7")
		dc.DrawStringAnchored(asTime(x).Format(tickFormat), px(x), 397, .5, 0)
	}
	for i, line := range lines {
		color := palette[i%len(palette)]
		dc.SetHexColor(color)
		dc.SetLineWidth(4)
		length := 0
		var last point
		stroke := func() {
			if length == 0 {
				return
			}
			dc.Stroke()
			dc.DrawCircle(px(last.x), py(last.y), 4.5)
			dc.Fill()
			length = 0
		}
		for _, p := range line.points {
			if math.IsNaN(p.y) {
				stroke()
				continue
			}
			if length == 0 {
				dc.MoveTo(px(p.x), py(p.y))
			} else {
				dc.LineTo(px(p.x), py(p.y))
			}
			last = p
			length++
		}
		stroke()
	}
	setFont(regularFont, 23)
	shown := min(3, len(lines))
	legendWidth := float64(width-88) / float64(shown)
	if shown < len(lines) {
		legendWidth -= 40
	}
	for i, line := range lines[:shown] {
		x := 42 + float64(i)*legendWidth
		name := line.name
		if name == "" {
			if len(lines) == 1 {
				name = "Value"
			} else {
				name = fmt.Sprintf("Series %d", i+1)
			}
		}
		dc.SetHexColor(palette[i])
		dc.DrawRoundedRectangle(x, 441, 24, 5, 2)
		dc.Fill()
		dc.SetHexColor("#DAE3EF")
		available := legendWidth - 52
		if len(lines) == 1 {
			available -= 210
		}
		dc.DrawString(fit(dc, name, available), x+36, 452)
	}
	if len(lines) == 1 {
		points := lines[0].points
		for i := len(points) - 1; i >= 0; i-- {
			if !math.IsNaN(points[i].y) {
				dc.SetHexColor(palette[0])
				dc.DrawStringAnchored("Last   "+axisNumber(points[i].y), right, 452, 1, 0)
				break
			}
		}
	}
	if shown < len(lines) {
		dc.DrawStringAnchored(fmt.Sprintf("+%d", len(lines)-shown), right, 452, 1, 0)
	}
	var buf bytes.Buffer
	err := dc.EncodePNG(&buf)
	return buf.Bytes(), err
}
