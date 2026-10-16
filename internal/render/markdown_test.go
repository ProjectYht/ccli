package render

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
)

func TestWrapPlainEnds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		w    int
	}{
		{"indent wider than the column", strings.Repeat(" ", 60) + "text that has to wrap somewhere", 54},
		{"indent as wide as the column", strings.Repeat(" ", 20) + "abc def ghi", 20},
		{"wide rune in a one-cell column", "漢字かな", 1},
		{"indent leaves room for one wide rune only", strings.Repeat(" ", 9) + "漢字かな漢字", 10},
		{"no spaces", strings.Repeat("x", 500), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() { done <- wrapPlain(c.in, c.w) }()
			select {
			case out := <-done:
				for _, ln := range strings.Split(out, "\n") {
					// A rune wider than the column has to stand alone.
					if w := runewidth.StringWidth(ln); w > c.w && len([]rune(strings.TrimSpace(ln))) > 1 {
						t.Fatalf("line %q is %d cells, column is %d", ln, w, c.w)
					}
				}
				if strings.ReplaceAll(strings.Join(strings.Fields(out), ""), " ", "") != strings.Join(strings.Fields(c.in), "") {
					t.Fatalf("text changed: %q", out)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("wrapPlain did not return")
			}
		})
	}
}

func TestWrapPlainKeepsIndent(t *testing.T) {
	out := wrapPlain("    one two three four five six", 16)
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "    ") {
			t.Fatalf("continuation lost its indent: %q", out)
		}
	}
}
