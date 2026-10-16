package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lr00rl/ccli/internal/transcript"
)

func TestOldSchemaIsSetAsideNotDeleted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ccli.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.SetMeta("schema", "0")
	s.SetMeta("marker", "old history")
	s.Close()

	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Meta("schema"); v != SchemaVersion {
		t.Fatalf("schema = %q", v)
	}
	if v, _ := s.Meta("marker"); v != "" {
		t.Fatal("new database kept old contents")
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "ccli.db.schema-0.*"))
	var main string
	for _, k := range kept {
		if !strings.HasSuffix(k, "-wal") && !strings.HasSuffix(k, "-shm") {
			main = k
		}
	}
	if main == "" {
		t.Fatalf("old database not kept: %v", kept)
	}
	old, err := openDB(main)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if v, _ := old.Meta("marker"); v != "old history" {
		t.Fatalf("old database lost its contents: %q", v)
	}
}

// Two ccli runs that planned from the same offset must not both add the same
// bytes' counters.
func TestApplyFromStaleOffsetIsSkipped(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cost := func(string, string, transcript.Usage) (float64, bool) { return 0, false }
	u := Update{
		File:   File{Path: "/x/s1.jsonl", Account: "default", SessionID: "s1", ProjectKey: "-x", Kind: "main", Offset: 100},
		Result: transcript.Result{Lines: 3, ToolCalls: 2},
	}
	for i := 0; i < 2; i++ {
		if err := st.Apply(ctx, []Update{u}, cost); err != nil {
			t.Fatal(err)
		}
	}
	var lines, calls int
	if err := st.DB().QueryRow("SELECT lines, tool_calls FROM files").Scan(&lines, &calls); err != nil {
		t.Fatal(err)
	}
	if lines != 3 || calls != 2 {
		t.Fatalf("lines %d calls %d, want 3 and 2", lines, calls)
	}
}
