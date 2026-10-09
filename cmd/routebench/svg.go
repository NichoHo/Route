package main

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// A tiny SVG chart writer: enough for a line chart over time and a grouped
// bar chart, with no dependencies. The output is a standalone file on a white
// background, so it reads the same wherever it is opened.

const (
	chartW, chartH = 760, 340
	padL, padR     = 70, 20
	padT, padB     = 46, 56
)

var palette = []string{"#1f6feb", "#d1242f", "#1a7f37", "#8250df"}

type series struct {
	Name   string
	Points [][2]float64 // x, y
}

// niceMax rounds up to a 1, 2 or 5 times a power of ten, so axis ticks are
// readable. A non-positive value gives 1.
func niceMax(v float64) float64 {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

func fnum(v float64) string {
	if math.Abs(v) >= 100 || v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func svgOpen(title string) *strings.Builder {
	b := &strings.Builder{}
	fmt.Fprintf(b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" font-family="Helvetica, Arial, sans-serif" font-size="12">`+"\n", chartW, chartH, chartW, chartH)
	fmt.Fprintf(b, `<title>%s</title><rect width="100%%" height="100%%" fill="#ffffff"/>`+"\n", html.EscapeString(title))
	fmt.Fprintf(b, `<text x="%d" y="24" font-size="15" font-weight="bold" fill="#1f2328">%s</text>`+"\n", padL, html.EscapeString(title))
	return b
}

func axes(b *strings.Builder, xLabel, yLabel string, yMax float64) {
	plotW, plotH := float64(chartW-padL-padR), float64(chartH-padT-padB)
	for i := 0; i <= 5; i++ {
		y := float64(padT) + plotH - plotH*float64(i)/5
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#d0d7de"/>`+"\n", padL, y, chartW-padR, y)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" fill="#59636e">%s</text>`+"\n", padL-8, y+4, fnum(yMax*float64(i)/5))
	}
	fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" fill="#59636e">%s</text>`+"\n", padL+int(plotW)/2, chartH-12, html.EscapeString(xLabel))
	fmt.Fprintf(b, `<text transform="translate(16 %d) rotate(-90)" text-anchor="middle" fill="#59636e">%s</text>`+"\n", padT+int(plotH)/2, html.EscapeString(yLabel))
}

func legend(b *strings.Builder, names []string) {
	x := padL
	for i, n := range names {
		fmt.Fprintf(b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/><text x="%d" y="%d" fill="#1f2328">%s</text>`+"\n",
			x, 32, palette[i%len(palette)], x+18, 43, html.EscapeString(n))
		x += 30 + 7*len(n)
	}
}

// lineChart draws one or more series against a shared x axis (seconds).
func lineChart(title, xLabel, yLabel string, ss []series) string {
	xMax, yMax := 0.0, 0.0
	for _, s := range ss {
		for _, p := range s.Points {
			xMax, yMax = math.Max(xMax, p[0]), math.Max(yMax, p[1])
		}
	}
	xMax, yMax = math.Max(xMax, 1), niceMax(yMax)
	plotW, plotH := float64(chartW-padL-padR), float64(chartH-padT-padB)

	b := svgOpen(title)
	axes(b, xLabel, yLabel, yMax)
	for i := 0; i <= 5; i++ {
		x := float64(padL) + plotW*float64(i)/5
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="middle" fill="#59636e">%s</text>`+"\n", x, chartH-padB+18, fnum(xMax*float64(i)/5))
	}
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.Name
		var pts []string
		for _, p := range s.Points {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", float64(padL)+plotW*p[0]/xMax, float64(padT)+plotH-plotH*p[1]/yMax))
		}
		if len(pts) > 0 {
			fmt.Fprintf(b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`+"\n", palette[i%len(palette)], strings.Join(pts, " "))
		}
	}
	legend(b, names)
	b.WriteString("</svg>\n")
	return b.String()
}

type barGroup struct {
	Name   string
	Values []float64 // one per category
}

// barChart draws grouped bars: one cluster per category, one bar per group.
func barChart(title, yLabel string, categories []string, groups []barGroup) string {
	yMax := 0.0
	for _, g := range groups {
		for _, v := range g.Values {
			yMax = math.Max(yMax, v)
		}
	}
	yMax = niceMax(yMax)
	plotW, plotH := float64(chartW-padL-padR), float64(chartH-padT-padB)
	clusterW := plotW / float64(len(categories))
	barW := clusterW * 0.7 / float64(len(groups))

	b := svgOpen(title)
	axes(b, "batch window", yLabel, yMax)
	names := make([]string, len(groups))
	for gi, g := range groups {
		names[gi] = g.Name
		for ci, v := range g.Values {
			if math.IsNaN(v) {
				continue
			}
			h := plotH * v / yMax
			x := float64(padL) + clusterW*float64(ci) + clusterW*0.15 + barW*float64(gi)
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n", x, float64(padT)+plotH-h, barW-2, h, palette[gi%len(palette)])
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#1f2328">%s</text>`+"\n", x+barW/2-1, float64(padT)+plotH-h-4, fnum(v))
		}
	}
	for ci, c := range categories {
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="middle" fill="#59636e">%s</text>`+"\n", float64(padL)+clusterW*(float64(ci)+0.5), chartH-padB+18, html.EscapeString(c))
	}
	legend(b, names)
	b.WriteString("</svg>\n")
	return b.String()
}
