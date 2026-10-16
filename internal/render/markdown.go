package render

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

var (
	mdMu    sync.Mutex
	mdCache = map[int]*glamour.TermRenderer{}
)

// lightTerminal reports whether the terminal says it has a light background
// (COLORFGBG="fg;bg" with a light bg), or CCLI_THEME=light asks for it.
// Asking the terminal itself would race with a TUI reading the keyboard.
func lightTerminal() bool {
	switch os.Getenv("CCLI_THEME") {
	case "light":
		return true
	case "dark":
		return false
	}
	v := os.Getenv("COLORFGBG")
	if i := strings.LastIndexByte(v, ';'); i >= 0 {
		bg := v[i+1:]
		return bg == "7" || bg == "15"
	}
	return false
}

func mdRenderer(w int) *glamour.TermRenderer {
	mdMu.Lock()
	defer mdMu.Unlock()
	if r, ok := mdCache[w]; ok {
		return r
	}
	st := styles.DarkStyleConfig
	if lightTerminal() {
		st = styles.LightStyleConfig
	}
	// The caller indents and spaces blocks itself.
	zero := uint(0)
	st.Document.Margin = &zero
	st.Document.BlockPrefix, st.Document.BlockSuffix = "", ""
	// Headings are shown by weight and color, without the "##" marks.
	for _, h := range []*ansi.StyleBlock{&st.H2, &st.H3, &st.H4, &st.H5, &st.H6} {
		h.Prefix = ""
	}
	r, err := glamour.NewTermRenderer(glamour.WithStyles(st), glamour.WithWordWrap(w), glamour.WithTableWrap(true))
	if err != nil {
		return nil
	}
	// One renderer per width; resizing a terminal goes through many.
	if len(mdCache) >= 8 {
		clear(mdCache)
	}
	mdCache[w] = r
	return r
}

// Markdown renders text as markdown for a column w cells wide, each line
// prefixed with indent. Without color it wraps the text as it is.
func (t Term) Markdown(text, indent string, w int) string {
	// Tabs (common in code) would be measured as no width but drawn as
	// several cells.
	text = strings.TrimSpace(strings.ReplaceAll(text, "\t", "    "))
	if text == "" {
		return ""
	}
	if w < 20 {
		w = 20
	}
	out := ""
	if t.Color {
		if r := mdRenderer(w); r != nil {
			if s, err := r.Render(text); err == nil {
				out = strings.Trim(s, "\n")
			}
		}
	}
	if out == "" {
		out = wrapPlain(text, w)
	}
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		ln = trailingPad.ReplaceAllString(ln, "$1")
		if strings.TrimSpace(stripANSI(ln)) == "" {
			lines[i] = ""
			continue
		}
		// Code blocks are padded with styled spaces past the wrap width; cut
		// every line to the column so nothing spills over.
		if xansi.StringWidth(ln) > w {
			ln = xansi.Truncate(ln, w, "")
		}
		lines[i] = indent + ln
	}
	return collapseBlank(strings.Join(lines, "\n"))
}

// wrapPlain wraps each line of s at w display cells, keeping blank lines and
// leading indentation.
func wrapPlain(s string, w int) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimRight(ln, " \t\r")
		lead := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		if runewidth.StringWidth(lead) > w/2 {
			// An indent this deep leaves no room for text; wrap flush left.
			ln, lead = ln[len(lead):], ""
		}
		for runewidth.StringWidth(ln) > w {
			cut := runewidth.Truncate(ln, w, "")
			if i := strings.LastIndexByte(cut, ' '); i > len(lead)+w/3 {
				cut = cut[:i]
			}
			if len(cut) <= len(lead) {
				// Nothing fits past the indent (a rune wider than the
				// column); take one rune so every pass makes progress.
				_, n := utf8.DecodeRuneInString(ln[len(lead):])
				cut = ln[:len(lead)+n]
			}
			out = append(out, cut)
			ln = lead + strings.TrimLeft(ln[len(cut):], " ")
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// collapseBlank keeps at most one blank line in a row.
func collapseBlank(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := false
	for _, ln := range lines {
		if ln == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, ln)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// trailingPad matches the spaces glamour pads a line with, before any
// closing color codes.
var trailingPad = regexp.MustCompile(` +((?:\x1b\[[0-9;]*m)*)$`)
