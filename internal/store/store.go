// Package store keeps ccli's index in SQLite. Aggregates are stored per
// transcript file so a single file can be re-read without touching others.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lr00rl/ccli/internal/transcript"
)

// SchemaVersion changes when the tables change. A database with another
// version is set aside (renamed, never deleted) and a new one is built, because
// it may hold history of transcripts Claude Code has since deleted.
const SchemaVersion = "1"

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  account TEXT NOT NULL,
  session_id TEXT NOT NULL,
  project_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  agent_type TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  tool_use_id TEXT NOT NULL DEFAULT '',
  spawn_depth INTEGER NOT NULL DEFAULT 0,
  request_shape TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  mtime INTEGER NOT NULL DEFAULT 0,
  born INTEGER NOT NULL DEFAULT 0,
  offset INTEGER NOT NULL DEFAULT 0,
  head_hash TEXT NOT NULL DEFAULT '',
  parser INTEGER NOT NULL DEFAULT 0,
  prev_ts INTEGER NOT NULL DEFAULT 0,
  missing INTEGER NOT NULL DEFAULT 0,
  lines INTEGER NOT NULL DEFAULT 0,
  skipped INTEGER NOT NULL DEFAULT 0,
  typed_prompts INTEGER NOT NULL DEFAULT 0,
  other_prompts INTEGER NOT NULL DEFAULT 0,
  tool_calls INTEGER NOT NULL DEFAULT 0,
  active_ms INTEGER NOT NULL DEFAULT 0,
  first_ts INTEGER NOT NULL DEFAULT 0,
  last_ts INTEGER NOT NULL DEFAULT 0,
  cwd TEXT NOT NULL DEFAULT '',
  custom_title TEXT NOT NULL DEFAULT '',
  ai_title TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  first_prompt TEXT NOT NULL DEFAULT '',
  team_name TEXT NOT NULL DEFAULT '',
  agent_name TEXT NOT NULL DEFAULT '',
  bridge_session_id TEXT NOT NULL DEFAULT '',
  entrypoint TEXT NOT NULL DEFAULT '',
  version TEXT NOT NULL DEFAULT '',
  git_branch TEXT NOT NULL DEFAULT '',
  cost_state_usd REAL NOT NULL DEFAULT 0,
  has_cost_state INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS files_session ON files(account, session_id);
CREATE INDEX IF NOT EXISTS files_sid ON files(session_id);
CREATE TABLE IF NOT EXISTS responses(
  key TEXT PRIMARY KEY,
  file_id INTEGER NOT NULL,
  account TEXT NOT NULL,
  session_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  ts INTEGER NOT NULL,
  model TEXT NOT NULL,
  speed TEXT NOT NULL,
  input INTEGER NOT NULL,
  output INTEGER NOT NULL,
  cache_read INTEGER NOT NULL,
  cache_5m INTEGER NOT NULL,
  cache_1h INTEGER NOT NULL,
  web_search INTEGER NOT NULL,
  cost REAL,
  born INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS responses_ts ON responses(ts);
CREATE INDEX IF NOT EXISTS responses_session ON responses(account, session_id);
CREATE INDEX IF NOT EXISTS responses_file ON responses(file_id);
CREATE TABLE IF NOT EXISTS activity(file_id INTEGER NOT NULL, hour INTEGER NOT NULL, ms INTEGER NOT NULL, PRIMARY KEY(file_id, hour));
CREATE TABLE IF NOT EXISTS tools(file_id INTEGER NOT NULL, name TEXT NOT NULL, skill TEXT NOT NULL, n INTEGER NOT NULL, PRIMARY KEY(file_id, name, skill));
CREATE TABLE IF NOT EXISTS links(file_id INTEGER NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, PRIMARY KEY(file_id, kind, target));
CREATE TABLE IF NOT EXISTS processes(
  file_id INTEGER NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, model TEXT NOT NULL,
  input INTEGER NOT NULL, output INTEGER NOT NULL, cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL,
  web_search INTEGER NOT NULL, cost REAL NOT NULL,
  PRIMARY KEY(file_id, start_ms, model)
);
CREATE INDEX IF NOT EXISTS processes_start ON processes(start_ms);
CREATE TABLE IF NOT EXISTS dirty(account TEXT NOT NULL, session_id TEXT NOT NULL, PRIMARY KEY(account, session_id));
CREATE TABLE IF NOT EXISTS spawns(file_id INTEGER NOT NULL, tool_use_id TEXT NOT NULL, PRIMARY KEY(file_id, tool_use_id));
CREATE INDEX IF NOT EXISTS spawns_tool ON spawns(tool_use_id);
CREATE TABLE IF NOT EXISTS runs(
  account TEXT NOT NULL, session_id TEXT NOT NULL, run_id TEXT NOT NULL,
  launched INTEGER NOT NULL DEFAULT 0, started INTEGER NOT NULL DEFAULT 0,
  results INTEGER NOT NULL DEFAULT 0, failed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(account, session_id, run_id)
);
`

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// DefaultPath is $CCLI_DB, else $XDG_DATA_HOME/ccli/ccli.db, else
// ~/.local/share/ccli/ccli.db.
func DefaultPath() (string, error) {
	if p := os.Getenv("CCLI_DB"); p != "" {
		return p, nil
	}
	return filepath.Join(DataDir(), "ccli.db"), nil
}

// DataDir is where ccli keeps its own files.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "ccli")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ccli")
}

// Open opens (and if needed creates or rebuilds) the database.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if v, _ := s.Meta("schema"); v != "" && v != SchemaVersion {
		s.db.Close()
		kept := fmt.Sprintf("%s.schema-%s.%s", path, v, time.Now().Format("20060102-150405"))
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(path+suffix, kept+suffix); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
		if s, err = openDB(path); err != nil {
			return nil, err
		}
	}
	db := s.db
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.SetMeta("schema", SchemaVersion); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for read queries.
func (s *Store) DB() *sql.DB { return s.db }

// Meta reads a meta value ("" when unset).
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || err != nil && isNoTable(err) {
		return "", nil
	}
	return v, err
}

func isNoTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// SetMeta writes a meta value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// File is the indexing state of one transcript file.
type File struct {
	ID         int64
	Path       string
	Account    string
	SessionID  string
	ProjectKey string
	Kind       string
	AgentID    string
	RunID      string
	Size       int64
	MTime      int64
	Born       int64
	Offset     int64
	HeadHash   string
	Parser     int
	PrevTS     int64
	Missing    bool
}

// Meta about a subagent, from its .meta.json.
type AgentMeta struct {
	AgentType    string
	Description  string
	ToolUseID    string
	SpawnDepth   int
	RequestShape string
}

// Files returns every known file by path.
func (s *Store) Files() (map[string]File, error) {
	rows, err := s.db.Query(`SELECT id, path, account, session_id, project_key, kind, agent_id, run_id, size, mtime, offset, head_hash, parser, prev_ts, missing FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]File{}
	for rows.Next() {
		var f File
		var missing int
		if err := rows.Scan(&f.ID, &f.Path, &f.Account, &f.SessionID, &f.ProjectKey, &f.Kind, &f.AgentID, &f.RunID, &f.Size, &f.MTime, &f.Offset, &f.HeadHash, &f.Parser, &f.PrevTS, &missing); err != nil {
			return nil, err
		}
		f.Missing = missing != 0
		out[f.Path] = f
	}
	return out, rows.Err()
}

// CostFunc prices one response; ok is false when the model is unpriced.
type CostFunc func(model, speed string, u transcript.Usage) (float64, bool)

// Update is one parse result to apply to a file.
type Update struct {
	File   File  // identity and new size, mtime, offset, head hash, parser
	Reset  bool  // drop everything recorded for this file first
	From   int64 // offset the parse started at; the update is skipped if another run moved it
	Result transcript.Result
	Agent  *AgentMeta
}

// Apply writes a batch of updates in one transaction.
func (s *Store) Apply(ctx context.Context, ups []Update, cost CostFunc) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range ups {
		if err := apply(ctx, tx, &ups[i], cost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func apply(ctx context.Context, tx *sql.Tx, u *Update, cost CostFunc) error {
	f := &u.File
	if f.ID == 0 {
		// RETURNING gives the row's id whether it was inserted or, because another
		// run got there first, updated; LastInsertId would be stale in that case.
		if err := tx.QueryRowContext(ctx, `INSERT INTO files(path, account, session_id, project_key, kind, agent_id, run_id) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET account = excluded.account RETURNING id`, f.Path, f.Account, f.SessionID, f.ProjectKey, f.Kind, f.AgentID, f.RunID).Scan(&f.ID); err != nil {
			return err
		}
	}
	if !u.Reset {
		// Another ccli run may have read these bytes since this one planned:
		// the counters below add, so apply them only from the expected offset.
		res, err := tx.ExecContext(ctx, "UPDATE files SET offset = offset WHERE id = ? AND offset = ?", f.ID, u.From)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO dirty(account, session_id) VALUES(?, ?)", f.Account, f.SessionID); err != nil {
		return err
	}
	if u.Reset {
		for _, q := range []string{
			"DELETE FROM responses WHERE file_id = ?",
			"DELETE FROM activity WHERE file_id = ?",
			"DELETE FROM tools WHERE file_id = ?",
			"DELETE FROM links WHERE file_id = ?",
			"DELETE FROM processes WHERE file_id = ?",
			"DELETE FROM spawns WHERE file_id = ?",
		} {
			if _, err := tx.ExecContext(ctx, q, f.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET lines=0, skipped=0, typed_prompts=0, other_prompts=0, tool_calls=0, active_ms=0,
			first_ts=0, last_ts=0, cwd='', custom_title='', ai_title='', summary='', first_prompt='', team_name='', agent_name='',
			bridge_session_id='', entrypoint='', version='', git_branch='', cost_state_usd=0, has_cost_state=0 WHERE id = ?`, f.ID); err != nil {
			return err
		}
	}
	r := &u.Result
	// A file's creation time only moves earlier: a copy written over it later
	// (a fast-forward replaces the file) must not make it look newer than the
	// files it was copied to. Its responses carry the time kept here.
	if err := tx.QueryRowContext(ctx, `UPDATE files SET born = CASE WHEN ? > 0 AND (born = 0 OR ? < born) THEN ? ELSE born END
		WHERE id = ? RETURNING born`, f.Born, f.Born, f.Born, f.ID).Scan(&f.Born); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET size=?, mtime=?, offset=?, head_hash=?, parser=?, prev_ts=?, missing=0,
		lines = lines + ?, skipped = skipped + ?, typed_prompts = typed_prompts + ?, other_prompts = other_prompts + ?,
		tool_calls = tool_calls + ?, active_ms = active_ms + ?,
		first_ts = CASE WHEN first_ts = 0 OR (? > 0 AND ? < first_ts) THEN ? ELSE first_ts END,
		last_ts = MAX(last_ts, ?),
		cwd = CASE WHEN cwd = '' THEN ? ELSE cwd END,
		custom_title = CASE WHEN ? <> '' THEN ? ELSE custom_title END,
		ai_title = CASE WHEN ? <> '' THEN ? ELSE ai_title END,
		summary = CASE WHEN ? <> '' THEN ? ELSE summary END,
		first_prompt = CASE WHEN first_prompt = '' THEN ? ELSE first_prompt END,
		team_name = CASE WHEN ? <> '' THEN ? ELSE team_name END,
		agent_name = CASE WHEN ? <> '' THEN ? ELSE agent_name END,
		agent_id = CASE WHEN agent_id = '' THEN ? ELSE agent_id END,
		bridge_session_id = CASE WHEN ? <> '' THEN ? ELSE bridge_session_id END,
		entrypoint = CASE WHEN ? <> '' THEN ? ELSE entrypoint END,
		version = CASE WHEN ? <> '' THEN ? ELSE version END,
		git_branch = CASE WHEN ? <> '' THEN ? ELSE git_branch END,
		cost_state_usd = CASE WHEN ? THEN ? ELSE cost_state_usd END,
		has_cost_state = MAX(has_cost_state, ?)
		WHERE id = ?`,
		f.Size, f.MTime, f.Offset, f.HeadHash, f.Parser, r.State.PrevTS,
		r.Lines, r.Skipped, r.TypedPrompts, r.OtherPrompts, r.ToolCalls, r.ActiveMS,
		r.FirstTS, r.FirstTS, r.FirstTS, r.LastTS, r.Cwd,
		r.CustomTitle, r.CustomTitle, r.AITitle, r.AITitle, r.Summary, r.Summary, r.FirstPrompt,
		r.TeamName, r.TeamName, r.AgentName, r.AgentName, r.AgentID,
		r.BridgeSessionID, r.BridgeSessionID, r.Entrypoint, r.Entrypoint, r.Version, r.Version, r.GitBranch, r.GitBranch,
		r.HasCost, r.CostUSD, boolInt(r.HasCost), f.ID); err != nil {
		return err
	}
	if a := u.Agent; a != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET agent_type=?, description=?, tool_use_id=?, spawn_depth=?, request_shape=? WHERE id = ?`,
			a.AgentType, a.Description, a.ToolUseID, a.SpawnDepth, a.RequestShape, f.ID); err != nil {
			return err
		}
	}
	for _, resp := range r.Responses {
		var c sql.NullFloat64
		if v, ok := cost(resp.Model, resp.Speed, resp.Usage); ok {
			c = sql.NullFloat64{Float64: v, Valid: true}
		}
		us := resp.Usage
		// A response in several files (a copy or fork) belongs to the file
		// created first; a later line in the owning file may revise its usage.
		if _, err := tx.ExecContext(ctx, `INSERT INTO responses(key, file_id, account, session_id, kind, ts, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search, cost, born)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET file_id=excluded.file_id, account=excluded.account, session_id=excluded.session_id, kind=excluded.kind,
			  born=excluded.born, model=excluded.model, speed=excluded.speed, input=excluded.input, output=excluded.output,
			  cache_read=excluded.cache_read, cache_5m=excluded.cache_5m, cache_1h=excluded.cache_1h, web_search=excluded.web_search, cost=excluded.cost
			WHERE responses.file_id = excluded.file_id OR (excluded.born > 0 AND (responses.born = 0 OR excluded.born < responses.born))`,
			resp.Key, f.ID, f.Account, f.SessionID, f.Kind, resp.TS, resp.Model, resp.Speed,
			us.Input, us.Output, us.CacheRead, us.Cache5m, us.Cache1h, us.WebSearch, c, f.Born); err != nil {
			return err
		}
	}
	for hour, ms := range r.Activity {
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity(file_id, hour, ms) VALUES(?,?,?) ON CONFLICT(file_id, hour) DO UPDATE SET ms = ms + excluded.ms`, f.ID, hour, ms); err != nil {
			return err
		}
	}
	for _, kv := range sortedCounts(r.Tools) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools(file_id, name, skill, n) VALUES(?,?,'',?) ON CONFLICT(file_id, name, skill) DO UPDATE SET n = n + excluded.n`, f.ID, kv.k, kv.n); err != nil {
			return err
		}
	}
	for _, kv := range sortedCounts(r.Skills) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tools(file_id, name, skill, n) VALUES(?, 'Skill', ?, ?) ON CONFLICT(file_id, name, skill) DO UPDATE SET n = n + excluded.n`, f.ID, kv.k, kv.n); err != nil {
			return err
		}
	}
	for _, t := range r.ContinuedIn {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO links(file_id, kind, target) VALUES(?, 'continued-in', ?)`, f.ID, t); err != nil {
			return err
		}
	}
	for _, id := range r.Spawns {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO spawns(file_id, tool_use_id) VALUES(?, ?)`, f.ID, id); err != nil {
			return err
		}
	}
	for _, p := range r.Processes {
		// A newer snapshot of the same process replaces the older one.
		if _, err := tx.ExecContext(ctx, "DELETE FROM processes WHERE file_id = ? AND start_ms = ?", f.ID, p.Start); err != nil {
			return err
		}
		for _, m := range p.Models {
			if _, err := tx.ExecContext(ctx, `INSERT INTO processes(file_id, start_ms, end_ms, model, input, output, cache_read, cache_write, web_search, cost)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, f.ID, p.Start, p.End, m.Model, m.Input, m.Output, m.CacheRead, m.CacheWrite, m.WebSearch, m.CostUSD); err != nil {
				return err
			}
		}
	}
	return nil
}

type kv struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MarkMissing flags files that are gone from disk (or back again).
func (s *Store) MarkMissing(ctx context.Context, ids []int64, missing bool) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE files SET missing = ? WHERE id = ?", boolInt(missing), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Run is a workflow run's journal summary.
type Run struct {
	Account, SessionID, RunID          string
	Launched, Started, Results, Failed int
}

// PutRuns replaces workflow run summaries.
func (s *Store) PutRuns(ctx context.Context, runs []Run) error {
	if len(runs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range runs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs(account, session_id, run_id, launched, started, results, failed) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(account, session_id, run_id) DO UPDATE SET launched=excluded.launched, started=excluded.started, results=excluded.results, failed=excluded.failed`,
			r.Account, r.SessionID, r.RunID, r.Launched, r.Started, r.Results, r.Failed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Reprice recomputes every stored cost with a new price table, records the
// table's hash as meta "prices", and marks every session for reconciliation.
func (s *Store) Reprice(ctx context.Context, cost CostFunc, hash string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search FROM responses WHERE kind <> 'adjustment'`)
	if err != nil {
		return err
	}
	type upd struct {
		key string
		c   sql.NullFloat64
	}
	var ups []upd
	for rows.Next() {
		var key, model, speed string
		var u transcript.Usage
		if err := rows.Scan(&key, &model, &speed, &u.Input, &u.Output, &u.CacheRead, &u.Cache5m, &u.Cache1h, &u.WebSearch); err != nil {
			rows.Close()
			return err
		}
		var c sql.NullFloat64
		if v, ok := cost(model, speed, u); ok {
			c = sql.NullFloat64{Float64: v, Valid: true}
		}
		ups = append(ups, upd{key, c})
	}
	rows.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "UPDATE responses SET cost = ? WHERE key = ?")
	if err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := stmt.ExecContext(ctx, u.c, u.key); err != nil {
			return err
		}
	}
	// Every session's reconciliation depends on its transcript cost; record the
	// new table only together with the marks that redo them.
	if err := markAllDirty(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('prices', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", hash); err != nil {
		return err
	}
	return tx.Commit()
}

func markAllDirty(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dirty(account, session_id)
		SELECT DISTINCT f.account, f.session_id FROM processes p JOIN files f ON f.id = p.file_id`)
	return err
}

func openDB(path string) (*Store, error) {
	// Write transactions take the lock when they begin, so two ccli runs
	// cannot interleave a read of a file's offset with a write of it.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}
