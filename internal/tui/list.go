package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// item is one row of a list screen and what it previews.
type item struct {
	row    func(w int) string // the row's text, at most w cells
	search string             // lower-cased text the filter matches
	id     string             // stable across rebuilds, to keep the selection
	dim    bool               // turns nobody typed, and the like

	// load fetches what the preview shows; draw renders it. A nil load
	// means the row has no preview.
	load func() (any, error)
	draw func(v *viewer, data any, w int) string
	// open returns the screen Enter opens, or nil to focus the preview.
	open func(data any) *screen
	// children returns the screen "o" opens: the agents a turn or an agent
	// started. Nil when there are none.
	children func(data any) *screen
	// copy returns the text "y" puts on the clipboard (the prompt).
	copy func(data any) string
}

// list is a filtered, scrollable selection over items.
type list struct {
	all    []item
	shown  []int // indexes into all, after the filter
	cursor int   // index into shown
	offset int   // first shown row on screen
	filter string
}

func (l *list) set(items []item) {
	l.all = items
	l.apply()
}

func (l *list) apply() {
	l.shown = l.shown[:0]
	q := strings.ToLower(strings.TrimSpace(l.filter))
	for i, it := range l.all {
		if q == "" || strings.Contains(it.search, q) {
			l.shown = append(l.shown, i)
		}
	}
	if l.cursor >= len(l.shown) {
		l.cursor = max(0, len(l.shown)-1)
	}
}

// selected returns the index into all of the selected item, or -1.
func (l *list) selected() int {
	if len(l.shown) == 0 {
		return -1
	}
	return l.shown[l.cursor]
}

// selectIndex moves the cursor to the item with index i into all, if shown.
func (l *list) selectIndex(i int) {
	for c, idx := range l.shown {
		if idx == i {
			l.cursor = c
			return
		}
	}
}

func (l *list) move(d int) {
	if len(l.shown) == 0 {
		return
	}
	l.cursor = min(max(l.cursor+d, 0), len(l.shown)-1)
}

func (l *list) scrollInto(height int) {
	if height <= 0 {
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+height {
		l.offset = l.cursor - height + 1
	}
	if maxOff := max(0, len(l.shown)-height); l.offset > maxOff {
		l.offset = maxOff
	}
}

// view renders height rows, each exactly w cells wide.
func (l *list) view(w, height int, focused bool) []string {
	l.scrollInto(height)
	out := make([]string, 0, height)
	for r := 0; r < height; r++ {
		c := l.offset + r
		if c >= len(l.shown) {
			out = append(out, strings.Repeat(" ", w))
			continue
		}
		it := l.all[l.shown[c]]
		text := it.row(w - 2)
		if it.dim {
			text = dim(ansi.Strip(text))
		}
		mark := "  "
		if c == l.cursor {
			if focused {
				mark = accent("▌ ")
			} else {
				mark = dim("▌ ")
			}
			text = bold(ansi.Strip(text))
		}
		out = append(out, fitLine(mark+text, w))
	}
	return out
}

// fitLine cuts or pads s to exactly w cells.
func fitLine(s string, w int) string {
	n := ansi.StringWidth(s)
	if n > w {
		s = ansi.Truncate(s, w, "…")
		n = ansi.StringWidth(s)
	}
	return s + strings.Repeat(" ", max(0, w-n))
}

// Colors match the command line views: one accent, green for money, amber
// for warnings, grey for secondary text.
func paint(code, s string) string {
	if s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func dim(s string) string    { return paint("38;5;245", s) }
func bold(s string) string   { return paint("1", s) }
func accent(s string) string { return paint("38;5;75", s) }
func money(s string) string  { return paint("38;5;114", s) }
func warn(s string) string   { return paint("38;5;214", s) }
