package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lr00rl/ccli/internal/conv"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
)

// sessionsScreen lists sessions, newest first; the preview is `ccli show`.
func (v *viewer) sessionsScreen() (*screen, error) {
	page, err := v.src.Sessions(v.ctx, query.Filter{Loc: v.loc, Sort: "last"})
	if err != nil {
		return nil, err
	}
	s := newScreen("sessions", "")
	var items []item
	for _, row := range page.Sessions {
		row := row
		ref := row.Account + ":" + row.ID
		title := row.Title
		if title == "" {
			title = "(untitled)"
		}
		items = append(items, item{
			row: func(w int) string {
				when := fmt.Sprintf("%-9s", v.term(0).Ago(row.Last))
				cost := fmt.Sprintf("%8s", render.USD(row.Cost.USD))
				live := " "
				if row.Live {
					live = paint("38;5;203", "●")
				}
				return dim(when) + " " + money(cost) + " " + live + " " + dim(row.Account) + " " + title
			},
			search: strings.ToLower(title + " " + row.ID + " " + row.Cwd + " " + row.Account),
			load: func() (any, error) {
				d, err := v.src.Session(v.ctx, ref)
				if err != nil {
					return nil, err // never a nil *SessionDetail inside a non-nil any
				}
				return d, nil
			},
			draw: func(v *viewer, data any, w int) string {
				return v.capture(w, func(t render.Term) { t.Session(data.(*query.SessionDetail), false) })
			},
			open: func(any) *screen {
				s, err := v.turnsScreen(ref)
				if err != nil {
					v.status = err.Error()
					return nil
				}
				return s
			},
		})
	}
	s.list.set(items)
	s.hint = fmt.Sprintf("%d sessions", len(items))
	return s, nil
}

// turnsScreen lists a session's turns; the preview is the turn in full.
func (v *viewer) turnsScreen(ref string) (*screen, error) {
	l, err := v.src.Turns(v.ctx, ref)
	if err != nil {
		return nil, err
	}
	title := l.Session.Title
	if title == "" {
		title = query.ShortID(l.Session.ID)
	}
	s := newScreen(title, "turns")
	s.rebuild = func(v *viewer) []item { return v.turnItems(ref, l) }
	s.list.set(s.rebuild(v))
	s.hint = fmt.Sprintf("%d typed · %s", l.Human(), render.USD(l.Session.Cost.USD))
	// Start on the latest turn: that is usually the one being looked for.
	if n := len(s.list.shown); n > 0 && !v.sortCost {
		s.list.cursor = n - 1
	}
	return s, nil
}

func (v *viewer) turnItems(ref string, l *query.TurnList) []item {
	turns := make([]query.TurnRow, 0, len(l.Turns))
	for _, t := range l.Turns {
		if v.allTurns || t.Kind == "human" {
			turns = append(turns, t)
		}
	}
	if v.sortCost {
		sort.SliceStable(turns, func(i, j int) bool { return turns[i].Total() > turns[j].Total() })
	}
	var items []item
	for _, tr := range turns {
		tr := tr
		items = append(items, item{
			row: func(w int) string {
				cost := ""
				if c := tr.Total(); c > 0 {
					cost = render.USD(c)
				}
				head := tr.Headline
				if head == "" && tr.Images > 0 {
					head = "(image)"
				}
				if tag := map[string]string{"task": "task ", "peer": "peer ", "other": "system ", "start": "start "}[tr.Kind]; tag != "" {
					head = tag + head
				}
				return fmt.Sprintf("%4d ", tr.Seq) + dim(tr.At.In(v.loc).Format("01-02 15:04")) + " " + money(fmt.Sprintf("%7s", cost)) + " " + head
			},
			search: strings.ToLower(tr.Prompt),
			id:     fmt.Sprint(tr.Seq),
			dim:    tr.Kind != "human",
			load: func() (any, error) {
				ds, err := v.src.TurnRange(v.ctx, ref, tr.Seq, tr.Seq, true)
				if len(ds) == 0 {
					return nil, err
				}
				if err != nil && ds[0].Turn == nil {
					return ds[0], err
				}
				return ds[0], nil
			},
			draw: func(v *viewer, data any, w int) string {
				return v.capture(w, func(t render.Term) { t.Turn(data.(*query.TurnDetail), v.opt) })
			},
			children: func(data any) *screen {
				d, ok := data.(*query.TurnDetail)
				if !ok || len(d.Agents) == 0 && len(d.Runs) == 0 {
					return nil
				}
				return v.agentsScreen(ref, fmt.Sprintf("turn %d agents", tr.Seq), d.Agents, d.Runs)
			},
			copy: func(data any) string {
				_, rest := conv.SplitReminders(tr.Prompt)
				if d, ok := data.(*query.TurnDetail); ok && d.Turn != nil {
					if p := d.Turn.Prompt(); p != nil {
						_, rest = conv.SplitReminders(p.Text)
					}
				}
				return conv.PromptDisplay(rest)
			},
		})
	}
	return items
}

// agentsScreen lists subagents (as a tree) and workflow runs.
func (v *viewer) agentsScreen(ref, title string, nodes []query.AgentNode, runs []query.RunInfo) *screen {
	s := newScreen(title, "")
	var items []item
	var walk func(ns []query.AgentNode, prefix string)
	walk = func(ns []query.AgentNode, prefix string) {
		for i, n := range ns {
			n := n
			branch, next := "├ ", "│ "
			if i == len(ns)-1 {
				branch, next = "└ ", "  "
			}
			p := prefix + branch
			items = append(items, v.agentItem(ref, n, p))
			walk(n.Children, prefix+next)
		}
	}
	walk(nodes, "")
	for _, r := range runs {
		r := r
		name := r.Name
		if name == "" {
			name = r.ID
		}
		items = append(items, item{
			row: func(w int) string {
				return "workflow " + name + " " + dim(fmt.Sprintf("%d agents · %s", max(r.Started, r.Agents), render.USD(r.Cost)))
			},
			search: strings.ToLower("workflow " + name + " " + r.ID + " " + r.Summary),
			load: func() (any, error) {
				info, nodes, status, err := v.src.RunAgents(v.ctx, ref, r.ID)
				if err != nil {
					return nil, err
				}
				return runData{info, nodes, status}, nil
			},
			draw: func(v *viewer, data any, w int) string {
				d := data.(runData)
				return v.capture(w, func(t render.Term) { t.RunAgents(d.info, d.nodes, d.status, ref) })
			},
			open: func(data any) *screen {
				d := data.(runData)
				rs := newScreen(name, "workflow run")
				var its []item
				pw := 0
				for _, n := range d.nodes {
					pw = max(pw, len([]rune(n.Phase)))
				}
				for _, n := range d.nodes {
					it := v.agentItem(ref, n, "")
					st := d.status[n.AgentID]
					inner := it.row
					it.row = func(w int) string {
						mark := dim(fmt.Sprintf("%-10s", st))
						if st == "failed" {
							mark = warn(fmt.Sprintf("%-10s", st))
						}
						return mark + " " + dim(fmt.Sprintf("%-*s ", pw, n.Phase)) + inner(w-12-pw)
					}
					its = append(its, it)
				}
				rs.list.set(its)
				rs.hint = fmt.Sprintf("%d agents", len(its))
				return rs
			},
		})
	}
	s.list.set(items)
	var parts []string
	if n := len(items) - len(runs); n > 0 {
		parts = append(parts, render.Plural(n, "agent"))
	}
	if len(runs) > 0 {
		parts = append(parts, render.Plural(len(runs), "workflow run"))
	}
	s.hint = strings.Join(parts, ", ")
	return s
}

type runData struct {
	info   *query.RunInfo
	nodes  []query.AgentNode
	status map[string]string
}

func (v *viewer) agentItem(ref string, n query.AgentNode, prefix string) item {
	return item{
		row: func(w int) string {
			return dim(prefix) + n.Label + " " + dim(render.USD(n.Cost))
		},
		search: strings.ToLower(n.Label + " " + n.AgentID),
		load: func() (any, error) {
			// A log can come back with an error (its transcript is gone);
			// keep it then, but never a nil *AgentLog inside a non-nil any.
			a, err := v.src.Agent(v.ctx, ref, n.AgentID)
			if a == nil {
				return nil, err
			}
			return a, err
		},
		draw: func(v *viewer, data any, w int) string {
			return v.capture(w, func(t render.Term) { t.AgentLog(data.(*query.AgentLog), v.opt) })
		},
		children: func(data any) *screen {
			a, ok := data.(*query.AgentLog)
			if !ok || len(a.Agent.Children) == 0 {
				return nil
			}
			return v.agentsScreen(ref, "agents of "+query.ShortID(n.AgentID), a.Agent.Children, nil)
		},
		copy: func(data any) string {
			a, ok := data.(*query.AgentLog)
			if !ok {
				return ""
			}
			for _, t := range a.Turns {
				if p := t.Prompt(); p != nil {
					return p.Text
				}
			}
			return ""
		},
	}
}
