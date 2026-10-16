// Package index keeps the store in step with the transcripts on disk. It reads
// only bytes added since the last run and never writes into a config dir.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/pricing"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transcript"
)

// ParserVersion changes when parsing changes; files parsed by an older version
// are read again from the start.
const ParserVersion = 1

const headBytes = 4096

// Options tune a run.
type Options struct {
	Rebuild  bool                               // re-read every file from the start
	Workers  int                                // parse goroutines (default: CPUs)
	Progress func(done, total int, bytes int64) // called from the writer goroutine
}

// Stats describe a run.
type Stats struct {
	Files      int           `json:"files"`
	Parsed     int           `json:"parsed"`
	Reset      int           `json:"reset"`
	Missing    int           `json:"missing"`
	Bytes      int64         `json:"bytes"`
	Reconciled int           `json:"reconciled"`
	Errors     int           `json:"errors"`                // files that could not be read; retried next run
	FirstError string        `json:"first_error,omitempty"` // the first such error
	Duration   time.Duration `json:"duration"`
}

type job struct {
	file  store.File
	reset bool
	from  int64
	state transcript.State
	meta  string // path of the subagent's .meta.json, if any
}

// Run brings the store up to date for the given accounts.
func Run(ctx context.Context, st *store.Store, accts []account.Account, prices *pricing.Table, opt Options) (Stats, error) {
	start := time.Now()
	var stats Stats
	known, err := st.Files()
	if err != nil {
		return stats, err
	}
	var jobs []job
	var runs []store.Run
	seen := map[string]bool{}
	for _, a := range accts {
		found, rs, err := scan(a)
		if err != nil {
			return stats, err
		}
		runs = append(runs, rs...)
		for _, d := range found {
			seen[d.file.Path] = true
			stats.Files++
			if j, ok := plan(d, known[d.file.Path], opt.Rebuild); ok {
				jobs = append(jobs, j)
			}
		}
	}
	// Files that vanished keep their history; mark them missing.
	accSet := map[string]bool{}
	for _, a := range accts {
		accSet[a.Name] = true
	}
	var gone []int64
	for p, f := range known {
		if !seen[p] && !f.Missing && accSet[f.Account] {
			gone = append(gone, f.ID)
		}
	}
	stats.Missing = len(gone)
	if err := st.MarkMissing(ctx, gone, true); err != nil {
		return stats, err
	}
	if err := st.PutRuns(ctx, runs); err != nil {
		return stats, err
	}
	for _, j := range jobs {
		if j.reset {
			stats.Reset++
		}
	}
	// Each applied update marks its session in the same transaction, so if
	// this stops here the next run reconciles what this one changed.
	if err := parseAll(ctx, st, jobs, prices, opt, &stats); err != nil {
		return stats, err
	}
	if h := prices.Hash(); h != mustMeta(st, "prices") {
		if err := st.Reprice(ctx, prices.Cost, h); err != nil {
			return stats, err
		}
	}
	n, err := st.Reconcile(ctx, pricing.Normalize, prices.Weight)
	if err != nil {
		return stats, err
	}
	stats.Reconciled = n
	stats.Duration = time.Since(start)
	return stats, nil
}

func mustMeta(st *store.Store, k string) string {
	v, _ := st.Meta(k)
	return v
}

type discovered struct {
	file store.File
	meta string
}

// scan lists an account's transcript files and workflow journals.
func scan(a account.Account) ([]discovered, []store.Run, error) {
	root := filepath.Join(a.Dir, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var out []discovered
	var runs []store.Run
	add := func(path, session, key, kind, agent, run, meta string) {
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		out = append(out, discovered{file: store.File{
			Path: path, Account: a.Name, SessionID: session, ProjectKey: key, Kind: kind,
			AgentID: agent, RunID: run, Size: st.Size(), MTime: st.ModTime().UnixMilli(), Born: birth(path, st),
		}, meta: meta})
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		key := p.Name()
		dir := filepath.Join(root, key)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() && strings.HasSuffix(name, ".jsonl") {
				add(filepath.Join(dir, name), strings.TrimSuffix(name, ".jsonl"), key, "main", "", "", "")
				continue
			}
			if !e.IsDir() {
				continue
			}
			session := name
			sub := filepath.Join(dir, session, "subagents")
			agents, err := os.ReadDir(sub)
			if err != nil {
				continue
			}
			for _, ae := range agents {
				an := ae.Name()
				if !ae.IsDir() && strings.HasPrefix(an, "agent-") && strings.HasSuffix(an, ".jsonl") {
					id := strings.TrimSuffix(strings.TrimPrefix(an, "agent-"), ".jsonl")
					add(filepath.Join(sub, an), session, key, "subagent", id, "", filepath.Join(sub, "agent-"+id+".meta.json"))
				}
			}
			wfRoot := filepath.Join(sub, "workflows")
			wfs, err := os.ReadDir(wfRoot)
			if err != nil {
				continue
			}
			for _, w := range wfs {
				if !w.IsDir() {
					continue
				}
				run := w.Name()
				rdir := filepath.Join(wfRoot, run)
				files, err := os.ReadDir(rdir)
				if err != nil {
					continue
				}
				for _, fe := range files {
					fn := fe.Name()
					switch {
					case fn == "journal.jsonl":
						runs = append(runs, journal(filepath.Join(rdir, fn), a.Name, session, run))
					case strings.HasPrefix(fn, "agent-") && strings.HasSuffix(fn, ".jsonl"):
						id := strings.TrimSuffix(strings.TrimPrefix(fn, "agent-"), ".jsonl")
						add(filepath.Join(rdir, fn), session, key, "workflow", id, run, filepath.Join(rdir, "agent-"+id+".meta.json"))
					}
				}
			}
		}
	}
	return out, runs, nil
}

// journal counts a workflow run's events.
func journal(path, acct, session, run string) store.Run {
	r := store.Run{Account: acct, SessionID: session, RunID: run}
	b, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	for _, ln := range strings.Split(string(b), "\n") {
		var v struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ln), &v) != nil {
			continue
		}
		switch v.Type {
		case "launched":
			r.Launched++
		case "started":
			r.Started++
		case "result":
			r.Results++
		case "failed":
			r.Failed++
		}
	}
	return r
}

// plan decides what to read from a file.
func plan(d discovered, old store.File, rebuild bool) (job, bool) {
	f := d.file
	j := job{file: f, meta: d.meta}
	if old.ID == 0 {
		return j, true
	}
	j.file.ID = old.ID
	if rebuild || old.Parser != ParserVersion || f.Size < old.Offset {
		j.reset = true
		return j, true
	}
	if f.Size == old.Offset {
		if old.Missing {
			j.from, j.state = old.Offset, transcript.State{PrevTS: old.PrevTS}
			return j, true // reappeared unchanged: clear the missing flag
		}
		return j, false
	}
	if old.Offset > 0 {
		h, err := headHash(f.Path, old.Offset)
		if err != nil || h != old.HeadHash {
			j.reset = true
			return j, true
		}
	}
	j.from, j.state = old.Offset, transcript.State{PrevTS: old.PrevTS}
	return j, true
}

// headHash hashes the first headBytes of a file, or fewer when only that many
// had been parsed.
func headHash(path string, upto int64) (string, error) {
	n := int64(headBytes)
	if upto < n {
		n = upto
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

type parsed struct {
	up    store.Update
	bytes int64
	err   error
}

func parseAll(ctx context.Context, st *store.Store, jobs []job, prices *pricing.Table, opt Options, stats *Stats) error {
	if len(jobs) == 0 {
		return nil
	}
	workers := opt.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	in := make(chan job)
	out := make(chan parsed, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range in {
				out <- parseOne(j)
			}
		}()
	}
	go func() {
		defer close(in)
		for _, j := range jobs {
			select {
			case in <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	var batch []store.Update
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := st.Apply(ctx, batch, prices.Cost)
		batch, batchBytes = batch[:0], 0
		return err
	}
	done := 0
	var firstErr error // only store errors and cancellation stop a run
	for p := range out {
		done++
		if p.err != nil {
			// An unreadable file keeps its old offset and is tried again.
			if !errors.Is(p.err, os.ErrNotExist) {
				stats.Errors++
				if stats.FirstError == "" {
					stats.FirstError = p.err.Error()
				}
			}
			continue
		}
		stats.Parsed++
		stats.Bytes += p.bytes
		batch = append(batch, p.up)
		batchBytes += p.bytes
		if len(batch) >= 64 || batchBytes >= 64<<20 {
			if err := flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if opt.Progress != nil {
			opt.Progress(done, len(jobs), stats.Bytes)
		}
	}
	if err := flush(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

func parseOne(j job) parsed {
	f, err := os.Open(j.file.Path)
	if err != nil {
		return parsed{err: err}
	}
	defer f.Close()
	from := j.from
	if j.reset {
		from = 0
		j.state = transcript.State{}
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return parsed{err: err}
	}
	kind := transcript.Main
	switch j.file.Kind {
	case "subagent":
		kind = transcript.Subagent
	case "workflow":
		kind = transcript.Workflow
	}
	res, err := transcript.Parse(f, kind, j.state)
	if err != nil {
		return parsed{err: err}
	}
	up := store.Update{File: j.file, Reset: j.reset, From: from, Result: res}
	up.File.Offset = from + res.Consumed
	up.File.Parser = ParserVersion
	if up.File.Offset > 0 {
		if h, err := headHash(j.file.Path, up.File.Offset); err == nil {
			up.File.HeadHash = h
		}
	}
	if j.meta != "" {
		up.Agent = readMeta(j.meta)
	}
	return parsed{up: up, bytes: res.Consumed}
}

func readMeta(path string) *store.AgentMeta {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v struct {
		AgentType    string `json:"agentType"`
		Description  string `json:"description"`
		ToolUseID    string `json:"toolUseId"`
		SpawnDepth   int    `json:"spawnDepth"`
		RequestShape string `json:"requestShape"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return &store.AgentMeta{AgentType: v.AgentType, Description: v.Description, ToolUseID: v.ToolUseID, SpawnDepth: v.SpawnDepth, RequestShape: v.RequestShape}
}
