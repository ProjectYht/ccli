package conv

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func ts(min float64) string {
	return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(min * float64(time.Minute))).Format(time.RFC3339Nano)
}

func user(min float64, text, extra string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}%s}`, ts(min), text, extra)
}

func asstBlock(min float64, id, block string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","content":[%s]}}`, ts(min), id, block)
}

func result(min float64, id, out, extra string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q%s,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, ts(min), extra, id, out)
}

func readLines(t *testing.T, sub bool, lines ...string) []Turn {
	t.Helper()
	turns, err := Read(strings.NewReader(strings.Join(lines, "\n")+"\n"), 0, 1, sub)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

func TestTurnsPairCallsAndResults(t *testing.T) {
	turns := readLines(t, false,
		`{"type":"system","subtype":"informational","content":"started","timestamp":"`+ts(0)+`"}`,
		user(1, "Fix **the** bug", `,"origin":{"kind":"human"}`),
		asstBlock(2, "m1", `{"type":"thinking","thinking":"look first"}`),
		asstBlock(2, "m1", `{"type":"text","text":"Reading it."}`),
		asstBlock(2, "m1", `{"type":"text","text":"Reading it."}`), // repeated line
		asstBlock(2, "m1", `{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"go test ./...\nmore","description":"run tests"}}`),
		result(3, "tu1", "ok", ""),
		asstBlock(4, "m2", `{"type":"tool_use","id":"tu2","name":"Agent","input":{"description":"review","subagent_type":"code-reviewer","prompt":"Review the diff"}}`),
		result(4.1, "tu2", "launched", `,"toolUseResult":{"agentId":"a9"}`),
		`{"type":"assistant","isSidechain":true,"timestamp":"`+ts(4.2)+`","message":{"id":"x","content":[{"type":"text","text":"inline subagent text"}]}}`,
		`{"type":"attachment","timestamp":"`+ts(4.5)+`","attachment":{"type":"queued_command","prompt":"also check the docs","commandMode":"prompt"}}`,
		asstBlock(5, "m3", `{"type":"text","text":"Done."}`),
		user(6, "<command-name>/model</command-name>\n<command-args>opus</command-args>", ""),
		user(6.1, "<local-command-stdout>Set model to opus</local-command-stdout>", ""),
	)
	if len(turns) != 3 {
		t.Fatalf("turns = %d", len(turns))
	}
	if turns[0].Seq != 0 || len(turns[0].Entries) != 1 || turns[0].Entries[0].Kind != KindNotice {
		t.Fatalf("turn 0 = %+v", turns[0])
	}
	t1 := turns[1]
	var kinds []string
	for _, e := range t1.Entries {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, " "); got != "prompt thinking text tool tool queued text" {
		t.Fatalf("turn 1 entries = %s", got)
	}
	calls := t1.Calls()
	if calls[0].Summary != "go test ./... …" || calls[0].Output != "ok" || !calls[0].Done {
		t.Fatalf("bash call = %+v", calls[0])
	}
	if calls[1].Summary != "code-reviewer: review" || calls[1].Target != "a9" || calls[1].Prompt != "Review the diff" {
		t.Fatalf("agent call = %+v", calls[1])
	}
	if p := t1.Prompt(); p == nil || p.Label != "human" || p.Text != "Fix **the** bug" {
		t.Fatalf("prompt = %+v", p)
	}
	t2 := turns[2]
	if t2.Seq != 2 || PromptDisplay(t2.Prompt().Text) != "/model opus" {
		t.Fatalf("turn 2 = %+v", t2)
	}
	if e := t2.Entries[len(t2.Entries)-1]; e.Kind != KindContext || e.Text != "Set model to opus" {
		t.Fatalf("command output = %+v", e)
	}
}

func TestSubagentFirstPromptIsAssigned(t *testing.T) {
	turns := readLines(t, true,
		user(0, "Audit the module", `,"isSidechain":true`),
		asstBlock(1, "s1", `{"type":"text","text":"Auditing."}`),
		user(2, "Also the tests", `,"isSidechain":true`),
	)
	if len(turns) != 2 || turns[0].Kind != KindAssigned || turns[1].Kind != "human" {
		t.Fatalf("turns = %+v", turns)
	}
	if len(turns[0].Entries) != 2 {
		t.Fatalf("sidechain lines of a subagent were dropped: %+v", turns[0].Entries)
	}
}

func TestPromptDisplay(t *testing.T) {
	cases := map[string]string{
		"<command-message>memhub:login</command-message>\n<command-name>/memhub:login</command-name>":                                                                             "/memhub:login",
		"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n<summary>Agent \"Review\" completed</summary>\n<result>All good.</result>\n</task-notification>": "Agent \"Review\" completed\n\nAll good.",
		"Another Claude session sent a message:\n<teammate-message teammate_id=\"researcher\" color=\"blue\">Found it.</teammate-message>":                                        "From researcher:\n\nFound it.",
		"plain *markdown*": "plain *markdown*",
	}
	for in, want := range cases {
		if got := PromptDisplay(in); got != want {
			t.Errorf("PromptDisplay(%q) = %q, want %q", in, got, want)
		}
	}
	if h := Headline("\n\n  first   line  \nsecond"); h != "first line" {
		t.Fatalf("headline = %q", h)
	}
}

func TestSummaries(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"Read", `{"file_path":"/a/b.go","offset":40,"limit":20}`, "/a/b.go:40"},
		{"Grep", `{"pattern":"TODO","path":"internal"}`, "TODO  in internal"},
		{"Workflow", `{"script":"export const meta = {\n  name: 'review-lanes',\n}"}`, "review-lanes"},
		{"Workflow", `{"scriptPath":"/x/workflows/scripts/s1-wave2-wf_f13df694-7ae.js","resumeFromRunId":"wf_f13df694-7ae"}`, "s1-wave2  (resumes wf_f13df694-7ae)"},
		{"SendMessage", `{"to":"reviewer","summary":"status"}`, "to reviewer: status"},
		{"mcp__claude-in-chrome__navigate", `{"tabId":3,"url":"https://example.com"}`, "https://example.com"},
		{"Bash", `{"command":"sleep 5","run_in_background":true}`, "sleep 5  (background)"},
	}
	for _, c := range cases {
		if got := Summary(c.name, []byte(c.input)); got != c.want {
			t.Errorf("Summary(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSplitReminders(t *testing.T) {
	rs, rest := SplitReminders("<system-reminder>\nnow in the desktop app\n</system-reminder>\n<!-- attach -->\n> quoted\n\nmy question")
	if len(rs) != 1 || rs[0] != "now in the desktop app" || rest != "> quoted\n\nmy question" {
		t.Fatalf("reminders=%q rest=%q", rs, rest)
	}
	if h := Headline("<system-reminder>x</system-reminder>\n\nreal prompt"); h != "real prompt" {
		t.Fatalf("headline = %q", h)
	}
}

func TestPastes(t *testing.T) {
	in := "review this\n\n<pasted_content id=\"ab\">\nline 1\n```go\nx\n```\n</pasted_content id=\"ab\">"
	if h := Headline(in); h != "review this" {
		t.Fatalf("headline = %q", h)
	}
	if h := Headline("<pasted_content id=\"ab\">\n\n  first pasted line\nmore\n</pasted_content id=\"ab\">"); h != "[pasted] first pasted line" {
		t.Fatalf("headline of a bare paste = %q", h)
	}
	if out := PromptDisplay(in); !strings.Contains(out, "> line 1  \n") {
		t.Fatalf("prose paste = %q", out)
	}
	log := "<pasted_content id=\"c\">\n$ go test\n  --- FAIL: TestX\n  ```\n</pasted_content id=\"c\">"
	if out := PromptDisplay(log); !strings.Contains(out, "````text\n$ go test\n  --- FAIL: TestX\n  ```\n````") {
		t.Fatalf("output paste = %q", out)
	}
	if h := Headline("<pasted_content id=\"a\">\nmy question\n</pasted_content id=\"a\">\n\nthen do this"); h != "[pasted] my question" {
		t.Fatalf("headline in reading order = %q", h)
	}
}
