package index

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transcript"
)

func at(min float64) string {
	return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(min * float64(time.Minute))).Format(time.RFC3339Nano)
}

func asst(min float64, id string, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/work/proj","message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":%d}}}`+"\n", at(min), id, out)
}

func prompt(min float64, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"cwd":"/work/proj","message":{"content":%q}}`+"\n", at(min), text)
}

type env struct {
	t      *testing.T
	accts  []account.Account
	st     *store.Store
	prices *pricing.Table
}

func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	root := t.TempDir()
	var accts []account.Account
	for _, n := range names {
		dir := filepath.Join(root, ".claude")
		if n != "default" {
			dir += "-" + n
		}
		os.MkdirAll(filepath.Join(dir, "projects"), 0o755)
		accts = append(accts, account.Account{Name: n, Dir: dir})
	}
	st, err := store.Open(filepath.Join(root, "ccli.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p, _ := pricing.Load("")
	return &env{t: t, accts: accts, st: st, prices: p}
}

func (e *env) path(acct int, rel string) string {
	return filepath.Join(e.accts[acct].Dir, "projects", rel)
}

func (e *env) write(acct int, rel, content string) {
	p := e.path(acct, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) appendTo(acct int, rel, content string) {
	f, err := os.OpenFile(e.path(acct, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

func (e *env) run() Stats {
	e.t.Helper()
	s, err := Run(context.Background(), e.st, e.accts, e.prices, Options{Workers: 2})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) int(q string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestFirstRunAndHierarchy(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-work-proj/s1.jsonl", prompt(0, "hello")+asst(1, "m1", 10)+asst(1, "m1", 10)+asst(2, "m2", 20))
	e.write(0, "-work-proj/s1/subagents/agent-a1.jsonl", asst(1.5, "sub1", 5))
	e.write(0, "-work-proj/s1/subagents/agent-a1.meta.json", `{"agentType":"code-reviewer","description":"review","toolUseId":"tu_9","spawnDepth":1,"requestShape":"background"}`)
	e.write(0, "-work-proj/s1/subagents/workflows/run1/agent-w1.jsonl", asst(3, "wf1", 7))
	e.write(0, "-work-proj/s1/subagents/workflows/run1/journal.jsonl", `{"type":"launched"}`+"\n"+`{"type":"started"}`+"\n"+`{"type":"result"}`+"\n")
	e.write(0, "-work-proj/s1/tool-results/big.txt", "ignored")
	s := e.run()
	if s.Files != 3 || s.Parsed != 3 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id='s1'"); n != 4 {
		t.Fatalf("responses = %d, want 4", n)
	}
	if n := e.int("SELECT SUM(output) FROM responses"); n != 42 {
		t.Fatalf("output = %d", n)
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE kind='subagent' AND agent_type='code-reviewer' AND tool_use_id='tu_9' AND spawn_depth=1"); n != 1 {
		t.Fatal("subagent meta not stored")
	}
	if n := e.int("SELECT COUNT(*) FROM files WHERE kind='workflow' AND run_id='run1' AND agent_id='w1'"); n != 1 {
		t.Fatal("workflow file not stored")
	}
	if n := e.int("SELECT launched+started+results FROM runs WHERE run_id='run1'"); n != 3 {
		t.Fatalf("journal = %d", n)
	}
	if n := e.int("SELECT typed_prompts FROM files WHERE kind='main'"); n != 1 {
		t.Fatalf("prompts = %d", n)
	}
	// A second run with nothing new reads nothing.
	if s := e.run(); s.Parsed != 0 || s.Bytes != 0 {
		t.Fatalf("idle run parsed %+v", s)
	}
}

func TestAppendReadsOnlyNewBytes(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", prompt(0, "a")+asst(2, "m1", 1))
	e.run()
	more := asst(4, "m2", 1) + asst(4.5, "m2", 1)
	e.appendTo(0, "-p/s1.jsonl", more)
	s := e.run()
	if s.Parsed != 1 || s.Bytes != int64(len(more)) || s.Reset != 0 {
		t.Fatalf("append stats %+v (want %d bytes)", s, len(more))
	}
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 2 {
		t.Fatalf("responses = %d", n)
	}
	// Active time continues across runs: 2 min + 2 min + 0.5 min.
	if n := e.int("SELECT active_ms FROM files"); n != 270_000 {
		t.Fatalf("active = %d", n)
	}
}

func TestPartialLineWaits(t *testing.T) {
	e := newEnv(t, "default")
	line := asst(1, "m1", 1)
	e.write(0, "-p/s1.jsonl", line[:30])
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 0 {
		t.Fatal("partial line parsed")
	}
	e.write(0, "-p/s1.jsonl", line)
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatal("completed line not parsed")
	}
}

func TestRewriteResets(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 5)+asst(2, "m2", 5))
	e.run()
	// Same length or longer, different head: a rewrite.
	e.write(0, "-p/s1.jsonl", asst(1, "x1", 6)+asst(2, "x2", 6)+asst(3, "x3", 6))
	s := e.run()
	if s.Reset != 1 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT SUM(output) FROM responses"); n != 18 {
		t.Fatalf("output after rewrite = %d", n)
	}
	// Shrinking also resets.
	e.write(0, "-p/s1.jsonl", asst(1, "y1", 1))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatalf("responses after shrink = %d", n)
	}
}

func TestDeletedTranscriptKeepsHistory(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 9))
	e.run()
	os.Remove(e.path(0, "-p/s1.jsonl"))
	if s := e.run(); s.Missing != 1 {
		t.Fatalf("stats %+v", s)
	}
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 1 {
		t.Fatal("history dropped")
	}
	if n := e.int("SELECT missing FROM files"); n != 1 {
		t.Fatal("not marked missing")
	}
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 9))
	e.run()
	if n := e.int("SELECT missing FROM files"); n != 0 {
		t.Fatal("reappeared file still missing")
	}
}

func TestCopiedSessionCountedOnce(t *testing.T) {
	e := newEnv(t, "default", "work")
	content := asst(1, "m1", 10) + asst(2, "m2", 10)
	e.write(0, "-p/s1.jsonl", content)
	e.run()
	e.write(1, "-p/s1.jsonl", content+asst(3, "m3", 10))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses"); n != 3 {
		t.Fatalf("responses = %d", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE account='work'"); n != 1 {
		t.Fatalf("work owns %d, want only the new one", n)
	}
}

func TestRepriceOnTableChange(t *testing.T) {
	e := newEnv(t, "default")
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0))
	e.run()
	if n := e.int("SELECT CAST(cost * 100 AS INTEGER) FROM responses"); n != 400 {
		t.Fatalf("cost cents = %d", n)
	}
	over := filepath.Join(t.TempDir(), "prices.json")
	os.WriteFile(over, []byte(`{"claude-opus-5-5":{"input":1,"output":1}}`), 0o644)
	e.prices, _ = pricing.Load(over)
	e.run()
	if n := e.int("SELECT CAST(cost * 100 AS INTEGER) FROM responses"); n != 100 {
		t.Fatalf("repriced cents = %d", n)
	}
}

func costState(start int64, cost float64, out, read int64) string {
	return fmt.Sprintf(`{"type":"cost-state","startTime":%d,"totalCostUSD":%v,"modelUsage":{"claude-opus-5-5":{"inputTokens":2000000,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":0,"costUSD":%v}}}`+"\n", start, cost, out, read, cost)
}

// withMissing is the cost in cents of the two recorded calls ($8) plus the
// tokens Claude Code's costState fixture shows beyond them, at list price.
func withMissing(e *env) int64 {
	w, _ := e.prices.Weight("claude-opus-5-5", transcript.Usage{Output: 50_000, CacheRead: 1_000_000})
	return int64(math.Round((8 + w) * 100))
}

func TestReconcileWithCostState(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	// Two recorded calls: 2M input at $4/M = $8. Claude Code's own total for
	// the process says $10 with extra output and cache reads.
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+asst(2, "m2", 0)+costState(start, 10, 50_000, 1_000_000))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment'"); n != 1 {
		t.Fatalf("adjustments = %d", n)
	}
	if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses WHERE session_id='s1'"); n != withMissing(e) {
		t.Fatalf("session cost cents = %d, want %d", n, withMissing(e))
	}
	if n := e.int("SELECT output FROM responses WHERE kind='adjustment'"); n != 50_000 {
		t.Fatalf("adjustment output = %d", n)
	}
	if n := e.int("SELECT input FROM responses WHERE kind='adjustment'"); n != 0 {
		t.Fatalf("adjustment input = %d (recorded input already matches)", n)
	}
	// Re-running does not stack adjustments.
	e.appendTo(0, "-p/s1.jsonl", prompt(10, "more"))
	e.run()
	if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses WHERE session_id='s1'"); n != withMissing(e) {
		t.Fatalf("after rerun cents = %d", n)
	}
	// A process whose own total is below the recorded calls adds nothing.
	e.write(0, "-p/s2.jsonl", asst(1, "n1", 0)+asst(2, "n2", 0)+costState(start, 1, 0, 0))
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND session_id='s2'"); n != 0 {
		t.Fatalf("negative shortfall stored")
	}
}

// A copy repeats its original's cost-state line, while its responses belong to
// the original. The process's shortfall must be added once, whichever file is
// indexed first.
func TestCopiedCostStateCountedOnce(t *testing.T) {
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	content := asst(1, "m1", 0) + asst(2, "m2", 0) + costState(start, 10, 50_000, 1_000_000)
	for _, order := range []string{"original first", "copy first", "same run"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t, "default", "work")
			e.write(0, "-p/s1.jsonl", content)
			time.Sleep(20 * time.Millisecond) // distinct creation times
			e.write(1, "-p/s1.jsonl", content)
			all := e.accts
			switch order {
			case "original first":
				e.accts = all[:1]
				e.run()
			case "copy first":
				e.accts = all[1:]
				e.run()
			}
			e.accts = all
			e.run()
			e.run() // and stays put on a rerun
			if n := e.int("SELECT CAST(ROUND(SUM(cost) * 100) AS INTEGER) FROM responses"); n != withMissing(e) {
				t.Fatalf("total cents = %d, want %d", n, withMissing(e))
			}
			if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND account='default'"); n != 1 {
				t.Fatalf("the original's session holds %d adjustments, want 1", n)
			}
		})
	}
}

// A file that cannot be read must not stop the rest of the run, including the
// reconciliation of the files that were read.
func TestUnreadableFileDoesNotStopReconcile(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+asst(2, "m2", 0)+costState(start, 10, 50_000, 1_000_000))
	e.write(0, "-p/s2.jsonl", asst(1, "n1", 0))
	if err := os.Chmod(e.path(0, "-p/s2.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(e.path(0, "-p/s2.jsonl"), 0o644) })
	s := e.run()
	if s.Errors != 1 || s.FirstError == "" {
		t.Fatalf("errors = %d %q, want 1", s.Errors, s.FirstError)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment'"); n != 1 {
		t.Fatalf("adjustments = %d, want 1", n)
	}
	// Once readable, the file is picked up.
	os.Chmod(e.path(0, "-p/s2.jsonl"), 0o644)
	if s := e.run(); s.Errors != 0 {
		t.Fatalf("errors after fix = %d", s.Errors)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE session_id='s2'"); n != 1 {
		t.Fatalf("s2 responses = %d", n)
	}
}

// Claude Code's figure for a model ccli has no price for is not added as
// cost, matching how unpriced calls are left out everywhere else.
func TestUnpricedModelGetsNoAdjustmentCost(t *testing.T) {
	e := newEnv(t, "default")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	line := fmt.Sprintf(`{"type":"cost-state","startTime":%d,"totalCostUSD":3,"modelUsage":{"some-new-model":{"inputTokens":0,"outputTokens":500,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":3}}}`+"\n", start)
	e.write(0, "-p/s1.jsonl", asst(1, "m1", 0)+line)
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND output=500 AND cost IS NULL"); n != 1 {
		t.Fatalf("want one unpriced adjustment with the missing tokens")
	}
}

// A fast-forward copy back replaces the original's file with a newer one; the
// original must keep its usage and its process adjustment all the same.
func TestOwnershipSurvivesReplacedFile(t *testing.T) {
	e := newEnv(t, "default", "work")
	start := time.Date(2026, 10, 1, 8, 59, 0, 0, time.UTC).UnixMilli()
	content := asst(1, "m1", 0) + asst(2, "m2", 0) + costState(start, 10, 50_000, 1_000_000)
	e.write(0, "-p/s1.jsonl", content)
	e.run()
	time.Sleep(20 * time.Millisecond)
	e.write(1, "-p/s1.jsonl", content)
	e.run()
	time.Sleep(20 * time.Millisecond)
	tmp := e.path(0, "-p/.s1.tmp")
	if err := os.WriteFile(tmp, []byte(content+prompt(10, "more")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, e.path(0, "-p/s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	e.run()
	if n := e.int("SELECT COUNT(*) FROM responses WHERE account='default' AND kind <> 'adjustment'"); n != 2 {
		t.Fatalf("default owns %d responses, want 2", n)
	}
	if n := e.int("SELECT COUNT(*) FROM responses WHERE kind='adjustment' AND account='default'"); n != 1 {
		t.Fatalf("default holds %d adjustments, want 1", n)
	}
}
