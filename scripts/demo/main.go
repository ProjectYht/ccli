// Command demo writes a synthetic Claude Code home with two accounts, so ccli
// can be tried, and the README screenshots rendered, without real data.
//
//	go run ./scripts/demo -home /tmp/ccli-demo
//	HOME=/tmp/ccli-demo XDG_DATA_HOME=/tmp/ccli-demo/.local/share ccli stats
//
// Everything in it (accounts, projects, titles, ids) is made up. The output is
// deterministic for a given -now.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type project struct {
	acct, dir string // dir is relative to the home
	titles    []string
	model     string
}

var projects = []project{
	{"work", "acme/checkout-service", []string{
		"Add idempotency keys to checkout retries",
		"Fix flaky cart total test around daylight saving",
		"Profile the slow order export and add an index",
		"Rotate the payment webhook signing secret",
	}, "claude-opus-5-5"},
	{"work", "acme/web-dashboard", []string{
		"Migrate dashboard charts to the v2 metrics API",
		"Upgrade the dashboard to React 20",
		"Fix tooltip overflow on narrow screens",
	}, "claude-opus-5-5"},
	{"work", "acme/data-pipeline", []string{
		"Backfill missing invoice rows from the event log",
		"Split the pipeline DAG into hourly and daily jobs",
		"Write the runbook for the queue outage",
	}, "claude-sonnet-5"},
	{"work", "acme/mobile-app", []string{
		"Review the login flow for token refresh bugs",
		"Add offline sync to the mobile cart",
	}, "claude-opus-5-5"},
	{"default", "code/blog", []string{
		"Port the blog to a static site generator",
		"Add dark mode to the blog theme",
		"Write a post on terminal UI density",
	}, "claude-sonnet-5"},
	{"default", "code/photo-sync", []string{
		"Speed up thumbnail generation",
		"Fix EXIF date parsing for old scans",
	}, "claude-opus-5-5"},
	{"default", "code/home-lab", []string{
		"Set up a Tailscale exit node on the home server",
		"Back up the home lab with restic",
	}, "claude-fable-5-1"},
	{"default", "code/dotfiles", []string{
		"Trim zsh startup time",
	}, "claude-sonnet-5"},
}

var prompts = []string{
	"Look at the failing test and find the cause before changing anything.",
	"Run the suite again and show me only the failures.",
	"Good. Now write the migration and a test for it.",
	"Review the diff and tell me what could break in production.",
	"Check how the other services call this endpoint.",
	"That works. Clean up the naming and commit it.",
	"Why is this query slow? Explain the plan.",
	"Try the simpler approach first.",
}

var subagentTypes = []struct{ kind, desc, model string }{
	{"Explore", "Find every caller of the retry helper", "claude-haiku-4-5-20251001"},
	{"code-reviewer", "Review the migration for data loss", "claude-sonnet-5"},
	{"general-purpose", "Reproduce the flaky test 50 times", "claude-sonnet-5"},
	{"Explore", "Map the config loading path", "claude-haiku-4-5-20251001"},
}

var skills = []string{"code-review", "simplify", "security-review", "frontend-design"}

type gen struct {
	home   string
	rng    *rand.Rand
	now    time.Time
	spawns int // subagents written so far, to vary their kinds
}

func main() {
	home := flag.String("home", "", "directory to write the fake home into (required)")
	nowFlag := flag.String("now", "", "anchor time, RFC 3339 (default: now)")
	livePID := flag.Int("live-pid", 0, "pid to register as a running session in the work account")
	flag.Parse()
	if *home == "" {
		fmt.Fprintln(os.Stderr, "usage: demo -home DIR [-now RFC3339] [-live-pid PID]")
		os.Exit(2)
	}
	now := time.Now()
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		now = t
	}
	g := &gen{home: *home, rng: rand.New(rand.NewPCG(7, 11)), now: now}
	if err := g.run(*livePID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (g *gen) run(livePID int) error {
	g.must(g.writeJSON(filepath.Join(g.home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "alex@example.com", "billingType": "stripe_subscription"},
	}))
	g.must(g.writeJSON(filepath.Join(g.dir("work"), ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "alex@acme.example", "organizationName": "Acme Inc", "billingType": "stripe_subscription"},
	}))

	type made struct {
		s    *session
		path string
	}
	var all []made
	add := func(s *session) {
		all = append(all, made{s, g.write(s)})
	}

	// Ordinary sessions, newest last.
	for i := 0; i < 38; i++ {
		p := projects[g.rng.IntN(len(projects))]
		if i%5 == 0 {
			p = projects[i%2] // keep the busiest work projects busy
		}
		s := g.session(p, g.rng.IntN(29)+1)
		switch {
		case i%3 == 0:
			s.subs = 1 + g.rng.IntN(2)
		case i%7 == 0:
			s.subs = 3
			s.nested = true
		}
		if i%4 == 1 {
			s.skill = skills[g.rng.IntN(len(skills))]
		}
		if i%11 == 3 {
			s.title = "" // falls back to the first prompt
		}
		add(s)
	}

	// Today: a long work session with a team, a workflow run and calls the
	// transcript never recorded, plus the running one.
	lead := g.session(projects[0], 0)
	lead.start = g.now.Add(-7 * time.Hour)
	lead.title, lead.turns, lead.subs, lead.nested, lead.workflow = "Audit payment retries across services", 46, 3, true, true
	lead.skill, lead.shortfall = "code-review", 0.07
	add(lead)
	for i, name := range []string{"researcher-1", "researcher-2", "reviewer"} {
		t := g.session(projects[0], 0)
		t.start = lead.start.Add(time.Duration(10+i*4) * time.Minute)
		t.title, t.turns, t.model = "", 6+g.rng.IntN(5), "claude-sonnet-5"
		t.team, t.agent = "session-"+lead.id[:8], name
		add(t)
	}
	first := g.session(projects[2], 1)
	first.title, first.turns = "Investigate duplicate invoice events", 18
	second := g.session(projects[2], 1)
	second.start = first.start.Add(3 * time.Hour)
	second.title = "Investigate duplicate invoice events (continued)"
	first.continued = second.id
	add(first)
	add(second)
	running := g.session(projects[1], 0)
	running.start = g.now.Add(-25 * time.Minute)
	running.title, running.turns = "Fix tooltip overflow on narrow screens", 9
	add(running)

	// Sessions that also exist in the personal account, for ccli diff: one
	// identical copy, one copy the work account has since extended, and one
	// that both accounts extended differently.
	var workIdx []int
	for i, m := range all {
		if m.s.acct == "work" && m.s.team == "" && m.s != lead && m.s != running {
			workIdx = append(workIdx, i)
		}
	}
	for n, i := range workIdx[:3] {
		m := all[i]
		data, err := os.ReadFile(m.path)
		g.must(err)
		lines := strings.SplitAfter(string(data), "\n")
		switch n {
		case 1: // work kept going after the copy
			data = []byte(strings.Join(lines[:len(lines)*2/3], ""))
		case 2: // both kept going
			cut := strings.Join(lines[:len(lines)/2], "")
			extra := g.line(map[string]any{"type": "user", "timestamp": ts(m.s.start.Add(-time.Minute)), "sessionId": m.s.id,
				"cwd": m.s.cwd, "promptSource": "typed", "message": map[string]any{"role": "user", "content": "Try it on the personal laptop instead."}})
			data = []byte(cut + extra)
		}
		dst := filepath.Join(g.dir("default"), "projects", key(m.s.cwd), m.s.id+".jsonl")
		g.must(os.MkdirAll(filepath.Dir(dst), 0o755))
		g.must(os.WriteFile(dst, data, 0o644))
	}

	if livePID > 0 {
		g.must(g.writeJSON(filepath.Join(g.dir("work"), "sessions", strconv.Itoa(livePID)+".json"), map[string]any{
			"pid": livePID, "sessionId": running.id, "cwd": running.cwd, "status": "busy",
		}))
	}
	fmt.Printf("wrote %d sessions under %s\n", len(all), g.home)
	return nil
}

type session struct {
	acct, cwd, id, title, model string
	start                       time.Time
	turns, subs                 int
	nested, workflow            bool
	skill, team, agent          string
	continued                   string
	shortfall                   float64
}

func (g *gen) session(p project, daysAgo int) *session {
	day := time.Date(g.now.Year(), g.now.Month(), g.now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -daysAgo)
	hours := []int{9, 10, 10, 11, 11, 14, 15, 15, 16, 17, 21, 22, 23}
	start := day.Add(time.Duration(hours[g.rng.IntN(len(hours))])*time.Hour + time.Duration(g.rng.IntN(50))*time.Minute)
	if daysAgo == 0 {
		start = g.now.Add(-time.Duration(150+g.rng.IntN(60)) * time.Minute)
	}
	return &session{
		acct:  p.acct,
		cwd:   filepath.Join(g.home, p.dir),
		id:    g.uuid(),
		title: p.titles[g.rng.IntN(len(p.titles))],
		model: p.model,
		start: start,
		turns: 4 + g.rng.IntN(22),
	}
}

// totals accumulates usage per model, for the cost-state line.
type totals map[string]*[4]int64

func (t totals) add(model string, in, out, read, write int64) {
	if t[model] == nil {
		t[model] = &[4]int64{}
	}
	v := t[model]
	v[0] += in
	v[1] += out
	v[2] += read
	v[3] += write
}

// write writes a session's main transcript and its subagents, and returns the
// main transcript's path.
func (g *gen) write(s *session) string {
	base := filepath.Join(g.dir(s.acct), "projects", key(s.cwd))
	path := filepath.Join(base, s.id+".jsonl")
	var b strings.Builder
	sum := totals{}
	t := s.start
	ctx := int64(18_000 + g.rng.IntN(8_000))
	common := func(m map[string]any) map[string]any {
		m["sessionId"], m["cwd"], m["timestamp"] = s.id, s.cwd, ts(t)
		if s.team != "" {
			m["teamName"], m["agentName"] = s.team, s.agent
		}
		return m
	}
	if s.title != "" && s.team == "" {
		b.WriteString(g.line(map[string]any{"type": "ai-title", "aiTitle": s.title, "sessionId": s.id}))
	}
	spawnAt := map[int]bool{}
	for i := 0; i < s.subs; i++ {
		spawnAt[1+g.rng.IntN(max(1, s.turns-1))] = true
	}
	subN, wfDone := 0, false
	for turn := 0; turn < s.turns; turn++ {
		if t.After(g.now.Add(-3 * time.Minute)) {
			break // sessions from today stop before now
		}
		text := prompts[g.rng.IntN(len(prompts))]
		if turn == 0 && s.title == "" && s.team == "" {
			text = "Why does the nightly export take forty minutes now?"
		}
		src := "typed"
		if s.team != "" {
			src = "teammate"
		}
		b.WriteString(g.line(common(map[string]any{"type": "user", "promptSource": src,
			"message": map[string]any{"role": "user", "content": text}})))
		for r, n := 0, 1+g.rng.IntN(5); r < n; r++ {
			t = t.Add(time.Duration(4+g.rng.IntN(40)) * time.Second)
			tool, input := g.tool()
			id := g.toolID()
			if spawnAt[turn] && r == 0 && subN < s.subs {
				st := subagentTypes[g.spawns%len(subagentTypes)]
				g.spawns++
				tool, input = "Agent", map[string]any{"description": st.desc, "subagent_type": st.kind}
				g.subagent(s, base, id, st.kind, st.desc, st.model, t, 1, s.nested && subN == 0, sum)
				subN++
			}
			if s.skill != "" && turn == 1 && r == 0 {
				tool, input = "Skill", map[string]any{"skill": s.skill}
			}
			write := int64(600 + g.rng.IntN(3_400))
			out := int64(150 + g.rng.IntN(2_200))
			in := int64(1 + g.rng.IntN(12))
			usage := map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": ctx,
				"cache_creation_input_tokens": write,
				"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": write}}
			if g.rng.IntN(40) == 0 {
				usage["server_tool_use"] = map[string]any{"web_search_requests": 2}
			}
			sum.add(s.model, in, out, ctx, write)
			msg := map[string]any{"id": g.msgID(), "model": s.model, "role": "assistant", "usage": usage,
				"content": []any{
					map[string]any{"type": "text", "text": "Checking."},
					map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input},
				}}
			b.WriteString(g.line(common(map[string]any{"type": "assistant", "requestId": "req_" + g.hex(20), "message": msg})))
			t = t.Add(time.Duration(2+g.rng.IntN(20)) * time.Second)
			b.WriteString(g.line(common(map[string]any{"type": "user", "message": map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}}})))
			ctx += write
		}
		if s.workflow && !wfDone && turn == s.turns/2 {
			g.workflow(s, base, t, sum)
			wfDone = true
		}
		// The person reads and types; now and then they leave for a while.
		gap := time.Duration(20+g.rng.IntN(200)) * time.Second
		if g.rng.IntN(9) == 0 {
			gap = time.Duration(12+g.rng.IntN(40)) * time.Minute
		}
		t = t.Add(gap)
		if ctx > 380_000 {
			ctx = 40_000 // compacted
		}
	}
	if s.continued != "" {
		b.WriteString(g.line(map[string]any{"type": "continued-in", "continuedInSessionId": s.continued, "sessionId": s.id}))
	}
	if s.shortfall > 0 {
		usage, cost := map[string]any{}, 0.0
		for m, v := range sum {
			f := 1.0
			if m == s.model {
				f += s.shortfall
			}
			c := (float64(v[0])*4 + float64(v[1])*20 + float64(v[2])*0.2 + float64(v[3])*8) * f / 1e6
			usage[m] = map[string]any{"inputTokens": int64(float64(v[0]) * f), "outputTokens": int64(float64(v[1]) * f),
				"cacheReadInputTokens": int64(float64(v[2]) * f), "cacheCreationInputTokens": int64(float64(v[3]) * f),
				"webSearchRequests": 0, "costUSD": c}
			cost += c
		}
		b.WriteString(g.line(map[string]any{"type": "cost-state", "sessionId": s.id, "totalCostUSD": cost,
			"startTime": s.start.UnixMilli(), "modelUsage": usage}))
	}
	g.must(os.MkdirAll(base, 0o755))
	g.must(os.WriteFile(path, []byte(b.String()), 0o644))
	return path
}

// subagent writes one subagent transcript and its meta file. A nested one
// spawns a further agent of its own.
func (g *gen) subagent(s *session, base, toolUse, kind, desc, model string, t time.Time, depth int, nest bool, sum totals) {
	aid := g.hex(17)
	dir := filepath.Join(base, s.id, "subagents")
	var b strings.Builder
	ctx := int64(9_000)
	for r, n := 0, 4+g.rng.IntN(9); r < n; r++ {
		t = t.Add(time.Duration(3+g.rng.IntN(15)) * time.Second)
		tool, input := g.tool()
		id := g.toolID()
		if nest && r == 1 {
			tool, input = "Agent", map[string]any{"description": "Check the retry docs", "subagent_type": "Explore"}
			g.subagent(s, base, id, "Explore", "Check the retry docs", "claude-haiku-4-5-20251001", t, depth+1, false, sum)
		}
		write, out := int64(400+g.rng.IntN(2_000)), int64(100+g.rng.IntN(1_200))
		sum.add(model, 2, out, ctx, write)
		b.WriteString(g.line(map[string]any{"type": "assistant", "isSidechain": true, "sessionId": s.id, "cwd": s.cwd,
			"timestamp": ts(t), "requestId": "req_" + g.hex(20),
			"message": map[string]any{"id": g.msgID(), "model": model, "role": "assistant",
				"usage": map[string]any{"input_tokens": 2, "output_tokens": out, "cache_read_input_tokens": ctx,
					"cache_creation_input_tokens": write,
					"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": write, "ephemeral_1h_input_tokens": 0}},
				"content": []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input}}}}))
		ctx += write
	}
	g.must(os.MkdirAll(dir, 0o755))
	g.must(os.WriteFile(filepath.Join(dir, "agent-"+aid+".jsonl"), []byte(b.String()), 0o644))
	g.must(g.writeJSON(filepath.Join(dir, "agent-"+aid+".meta.json"), map[string]any{
		"agentType": kind, "description": desc, "toolUseId": toolUse, "spawnDepth": depth, "requestShape": "background",
	}))
}

// workflow writes a three-agent workflow run with its journal.
func (g *gen) workflow(s *session, base string, t time.Time, sum totals) {
	run := "wf_" + g.hex(12)
	dir := filepath.Join(base, s.id, "subagents", "workflows", run)
	g.must(os.MkdirAll(dir, 0o755))
	var journal strings.Builder
	for i := 0; i < 3; i++ {
		journal.WriteString(`{"type":"launched"}` + "\n")
	}
	for i, job := range []string{"Scan service A for retry loops", "Scan service B for retry loops", "Summarise the findings"} {
		journal.WriteString(`{"type":"started"}` + "\n")
		var b strings.Builder
		ctx := int64(7_000)
		for r := 0; r < 5+i; r++ {
			t = t.Add(time.Duration(5+g.rng.IntN(20)) * time.Second)
			write, out := int64(500+g.rng.IntN(1_500)), int64(200+g.rng.IntN(900))
			sum.add("claude-sonnet-5", 3, out, ctx, write)
			b.WriteString(g.line(map[string]any{"type": "assistant", "isSidechain": true, "sessionId": s.id, "cwd": s.cwd,
				"timestamp": ts(t), "message": map[string]any{"id": g.msgID(), "model": "claude-sonnet-5", "role": "assistant",
					"usage":   map[string]any{"input_tokens": 3, "output_tokens": out, "cache_read_input_tokens": ctx, "cache_creation_input_tokens": write},
					"content": []any{map[string]any{"type": "text", "text": "Done."}}}}))
			ctx += write
		}
		aid := g.hex(17)
		g.must(os.WriteFile(filepath.Join(dir, "agent-"+aid+".jsonl"), []byte(b.String()), 0o644))
		g.must(g.writeJSON(filepath.Join(dir, "agent-"+aid+".meta.json"), map[string]any{
			"agentType": "workflow", "description": job, "spawnDepth": 1,
		}))
		journal.WriteString(`{"type":"result"}` + "\n")
	}
	g.must(os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(journal.String()), 0o644))
}

func (g *gen) tool() (string, map[string]any) {
	switch n := g.rng.IntN(100); {
	case n < 34:
		return "Read", map[string]any{"file_path": "src/retry.go"}
	case n < 58:
		return "Bash", map[string]any{"command": "go test ./..."}
	case n < 78:
		return "Edit", map[string]any{"file_path": "src/retry.go"}
	case n < 90:
		return "Grep", map[string]any{"pattern": "Retry"}
	case n < 95:
		return "Glob", map[string]any{"pattern": "**/*.go"}
	default:
		return "Write", map[string]any{"file_path": "docs/notes.md"}
	}
}

func (g *gen) dir(acct string) string {
	if acct == "default" {
		return filepath.Join(g.home, ".claude")
	}
	return filepath.Join(g.home, ".claude-"+acct)
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// key is Claude Code's project folder name for a working directory.
func key(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (g *gen) hex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[g.rng.IntN(16)]
	}
	return string(b)
}

func (g *gen) uuid() string {
	h := g.hex(32)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}

func (g *gen) msgID() string  { return "msg_" + g.hex(24) }
func (g *gen) toolID() string { return "toolu_" + g.hex(22) }

func (g *gen) line(v map[string]any) string {
	b, err := json.Marshal(v)
	g.must(err)
	return string(b) + "\n"
}

func (g *gen) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func (g *gen) must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
