package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/quota"
)

// QuotaRow is one account's limits and recent usage.
type QuotaRow struct {
	Account  string          `json:"account"`
	Email    string          `json:"email,omitempty"`
	Snapshot *quota.Snapshot `json:"snapshot,omitempty"`
	Last5h   query.Tokens    `json:"tokens_last_5h"`
	Cost5h   query.Cost      `json:"cost_last_5h"`
	Last7d   query.Tokens    `json:"tokens_last_7d"`
	Cost7d   query.Cost      `json:"cost_last_7d"`
}

// Quota prints subscription limits per account.
func (t Term) Quota(rows []QuotaRow) {
	missing := false
	for i, r := range rows {
		if i > 0 {
			t.println()
		}
		head := t.bold(r.Account)
		if r.Email != "" {
			head += t.dim("  " + r.Email)
		}
		t.println(head)
		s := r.Snapshot
		if s == nil {
			missing = true
			t.printf("  %s\n", t.dim("no limits recorded yet"))
		} else {
			t.window("5-hour", s.FiveHour, s.CapturedAt)
			t.window("weekly", s.SevenDay, s.CapturedAt)
			t.printf("  %s\n", t.dim(fmt.Sprintf("reported by the status line %s", t.Ago(s.CapturedAt))))
		}
		t.printf("  %s %s tokens, %s cache read, %s list price\n", t.dim(pad("last 5h", 9, false)), Num(r.Last5h.InOut()), Num(r.Last5h.CacheRead), t.money(costText(r.Cost5h)))
		t.printf("  %s %s tokens, %s cache read, %s list price\n", t.dim(pad("last 7d", 9, false)), Num(r.Last7d.InOut()), Num(r.Last7d.CacheRead), t.money(costText(r.Cost7d)))
	}
	if missing {
		t.println()
		t.println(t.dim("Limits come from the status line. To record them, put \"ccli statusline --\" in front of"))
		t.println(t.dim("your statusLine command in settings.json (see ccli statusline -h)."))
	}
}

func (t Term) window(name string, w *quota.Window, captured time.Time) {
	if w == nil {
		t.printf("  %s %s\n", t.dim(pad(name, 9, false)), t.dim("not reported"))
		return
	}
	if w.Expired(t.Now) {
		t.printf("  %s %s\n", t.dim(pad(name, 9, false)), t.dim(fmt.Sprintf("reset at %s since the last report (was %.0f%%)", w.ResetsAt.In(t.Loc).Format("01-02 15:04"), w.UsedPercent)))
		return
	}
	const cells = 24
	frac := w.UsedPercent / 100
	b := bar(frac, cells)
	color := cAccent
	switch {
	case w.UsedPercent >= 90:
		color = cLive
	case w.UsedPercent >= 70:
		color = cWarn
	}
	left := w.ResetsAt.Sub(t.Now)
	t.printf("  %s %s%s %s  %s\n", t.dim(pad(name, 9, false)), t.paint(color, b), t.dim(strings.Repeat("·", cells-width(b))),
		t.bold(fmt.Sprintf("%3.0f%%", w.UsedPercent)),
		t.dim("resets in "+Dur(left.Milliseconds())+" ("+w.ResetsAt.In(t.Loc).Format("Mon 15:04")+")"))
}
