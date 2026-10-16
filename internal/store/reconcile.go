package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/lr00rl/ccli/internal/transcript"
)

// ReconcileVersion changes when the reconciliation rules change; a database
// reconciled under another version is redone in full.
const ReconcileVersion = "3"

// SessionKey names a session within an account.
type SessionKey struct {
	Account   string
	SessionID string
}

// Reconcile settles every session marked dirty. Claude Code's own per-process
// totals (cost-state) are compared with the responses recorded in the
// transcripts, and for each process and model the shortfall becomes one
// "adjustment" response dated at the process's end. A shortfall is never
// negative, so recorded calls are never subtracted.
//
// A copied or forked transcript repeats its original's cost-state lines, so
// one process can appear in several sessions. Its responses are counted in
// whichever of those sessions the store placed them, and its shortfall is
// added once, to the session of the earliest-created file holding it.
//
// The marks are cleared in the same transaction, so a run that stops early
// leaves them for the next one. normalize maps model names to price-table keys;
// weigh prices an aggregate at list prices, and its false result for an
// unpriced model leaves the adjustment without cost, as everywhere else.
func (s *Store) Reconcile(ctx context.Context, normalize func(string) string, weigh WeighFunc) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'reconcile'").Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if v != ReconcileVersion {
		if err := markAllDirty(ctx, tx); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('reconcile', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", ReconcileVersion); err != nil {
			return 0, err
		}
	}
	// Sessions sharing a process with a marked one are redone too: which of
	// them owns the process, and what each has recorded, may have changed.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO dirty(account, session_id)
		SELECT DISTINCT f2.account, f2.session_id FROM dirty d
		JOIN files f1 ON f1.account = d.account AND f1.session_id = d.session_id
		JOIN processes p1 ON p1.file_id = f1.id
		JOIN processes p2 ON p2.start_ms = p1.start_ms
		JOIN files f2 ON f2.id = p2.file_id`); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT account, session_id FROM dirty")
	if err != nil {
		return 0, err
	}
	var keys []SessionKey
	for rows.Next() {
		var k SessionKey
		if err := rows.Scan(&k.Account, &k.SessionID); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	for _, k := range keys {
		if err := reconcileOne(ctx, tx, k, normalize, weigh); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM dirty"); err != nil {
		return 0, err
	}
	return len(keys), tx.Commit()
}

// WeighFunc prices an aggregate of tokens at base list prices; false when
// the model is unpriced.
type WeighFunc func(model string, u transcript.Usage) (float64, bool)

// usage turns a sum into token counts. Claude Code reports cache writes as
// one number; they are weighed as 1-hour writes, the kind it makes.
func usage(s sums) transcript.Usage {
	return transcript.Usage{Input: s.input, Output: s.output, CacheRead: s.read, Cache1h: s.write, WebSearch: s.web}
}

type sums struct {
	input, output, read, write, web int64
	cost                            float64
	unpriced                        int
}

func (a *sums) add(b sums) {
	a.input += b.input
	a.output += b.output
	a.read += b.read
	a.write += b.write
	a.web += b.web
	a.cost += b.cost
	a.unpriced += b.unpriced
}

// snapshot is one file's latest cost-state record of a process.
type snapshot struct {
	fileID, end int64
	sess        SessionKey
	rank        int64 // creation order: birth time, else first line time
	models      map[string]sums
}

func reconcileOne(ctx context.Context, tx *sql.Tx, k SessionKey, normalize func(string) string, weigh WeighFunc) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM responses WHERE kind = 'adjustment' AND account = ? AND session_id = ?`, k.Account, k.SessionID); err != nil {
		return err
	}
	starts, err := int64s(ctx, tx, `SELECT DISTINCT p.start_ms FROM processes p JOIN files f ON f.id = p.file_id
		WHERE f.account = ? AND f.session_id = ? ORDER BY p.start_ms`, k.Account, k.SessionID)
	if err != nil {
		return err
	}
	for _, start := range starts {
		snaps, err := holders(ctx, tx, start, normalize)
		if err != nil {
			return err
		}
		owner, best := snaps[0], snaps[0]
		for _, sn := range snaps[1:] {
			if sn.rank < owner.rank || sn.rank == owner.rank && sn.fileID < owner.fileID {
				owner = sn
			}
		}
		if owner.sess != k {
			continue // another session's file holds the original
		}
		sessions := map[SessionKey]bool{}
		for _, sn := range snaps {
			sessions[sn.sess] = true
			// Snapshots are cumulative; a copy taken mid-run holds an earlier one.
			if sn.end > best.end || sn.end == best.end && sn == owner {
				best = sn
			}
		}
		end := best.end
		if end < start {
			end = start
		}
		have := map[string]sums{}
		for sess := range sessions {
			if err := recorded(ctx, tx, sess, start, end, normalize, have); err != nil {
				return err
			}
		}
		for model, want := range best.models {
			got := have[model]
			d := sums{
				input:  pos(want.input - got.input),
				output: pos(want.output - got.output),
				read:   pos(want.read - got.read),
				write:  pos(want.write - got.write),
				web:    pos(want.web - got.web),
			}
			if d.input+d.output+d.read+d.write+d.web == 0 {
				continue // nothing missing; a cost difference alone is a price difference
			}
			// The missing tokens are priced from the same table as every recorded
			// call (at base rates: which of them ran in fast mode or past the
			// long-context threshold is unknown), so a price override applies to
			// them too and Claude Code's own prices never leak in.
			part, ok := weigh(model, usage(d))
			costed := got.unpriced == 0 && ok
			if costed {
				d.cost = part
			}
			key := fmt.Sprintf("adj|%s|%s|%d|%s", k.Account, k.SessionID, start, model)
			if _, err := tx.ExecContext(ctx, `INSERT INTO responses(key, file_id, account, session_id, kind, ts, model, speed, input, output, cache_read, cache_5m, cache_1h, web_search, cost)
				VALUES(?,?,?,?,'adjustment',?,?,'standard',?,?,?,?,0,?,?)
				ON CONFLICT(key) DO UPDATE SET file_id=excluded.file_id, ts=excluded.ts, input=excluded.input, output=excluded.output,
				  cache_read=excluded.cache_read, cache_5m=excluded.cache_5m, web_search=excluded.web_search, cost=excluded.cost`,
				key, owner.fileID, k.Account, k.SessionID, end, model, d.input, d.output, d.read, d.write, d.web, nullCost(d.cost, costed)); err != nil {
				return err
			}
		}
	}
	return nil
}

// holders returns every file's snapshot of the process that started at start.
func holders(ctx context.Context, tx *sql.Tx, start int64, normalize func(string) string) ([]*snapshot, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.file_id, p.end_ms, p.model, p.input, p.output, p.cache_read, p.cache_write, p.web_search, p.cost,
		f.account, f.session_id, f.born, f.first_ts
		FROM processes p JOIN files f ON f.id = p.file_id WHERE p.start_ms = ? ORDER BY p.file_id`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*snapshot
	byFile := map[int64]*snapshot{}
	for rows.Next() {
		var fileID, end, born, first int64
		var model string
		var v sums
		var sess SessionKey
		if err := rows.Scan(&fileID, &end, &model, &v.input, &v.output, &v.read, &v.write, &v.web, &v.cost,
			&sess.Account, &sess.SessionID, &born, &first); err != nil {
			return nil, err
		}
		sn := byFile[fileID]
		if sn == nil {
			rank := born
			if rank == 0 {
				rank = first
			}
			if rank == 0 {
				rank = math.MaxInt64
			}
			sn = &snapshot{fileID: fileID, end: end, sess: sess, rank: rank, models: map[string]sums{}}
			byFile[fileID] = sn
			out = append(out, sn)
		}
		m := normalize(model)
		acc := sn.models[m]
		acc.add(v)
		sn.models[m] = acc
	}
	return out, rows.Err()
}

// recorded adds a session's transcript responses within [start, end] to have.
func recorded(ctx context.Context, tx *sql.Tx, sess SessionKey, start, end int64, normalize func(string) string, have map[string]sums) error {
	rows, err := tx.QueryContext(ctx, `SELECT model, input, output, cache_read, cache_5m + cache_1h, web_search, COALESCE(cost, 0), cost IS NULL
		FROM responses WHERE account = ? AND session_id = ? AND kind <> 'adjustment' AND ts BETWEEN ? AND ?`, sess.Account, sess.SessionID, start, end)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var v sums
		if err := rows.Scan(&model, &v.input, &v.output, &v.read, &v.write, &v.web, &v.cost, &v.unpriced); err != nil {
			return err
		}
		m := normalize(model)
		acc := have[m]
		acc.add(v)
		have[m] = acc
	}
	return rows.Err()
}

func int64s(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func nullCost(c float64, ok bool) sql.NullFloat64 {
	return sql.NullFloat64{Float64: c, Valid: ok}
}

func pos(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
