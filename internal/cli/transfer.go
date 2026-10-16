package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/query"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
	"github.com/lr00rl/ccli/internal/transfer"
)

func init() {
	commands["diff"] = cmdDiff
	commands["copy"] = func(ctx context.Context, a *app, args []string) error {
		return cmdTransfer(ctx, a, args, transfer.Copy)
	}
	commands["move"] = func(ctx context.Context, a *app, args []string) error {
		return cmdTransfer(ctx, a, args, transfer.Move)
	}
}

func (a *app) account(name string) (account.Account, error) { return account.Find(a.accts, name) }

// sessionInfo maps account/session to its list row, for titles and activity.
func sessionInfo(ctx context.Context, svc *query.DB, accts ...string) (map[string]query.SessionRow, error) {
	page, err := svc.Sessions(ctx, query.Filter{Accounts: accts})
	if err != nil {
		return nil, err
	}
	out := map[string]query.SessionRow{}
	for _, s := range page.Sessions {
		out[s.Account+"/"+s.ID] = s
	}
	return out, nil
}

func cmdDiff(ctx context.Context, a *app, args []string) error {
	var c common
	fs := newFlags("diff", a, &c)
	project := fs.String("project", "", "only projects whose folder name contains this")
	all := fs.Bool("all", false, "include identical sessions")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: ccli diff <account a> <account b>")
	}
	svc, err := a.prepare(ctx, c, pos)
	if err != nil {
		return err
	}
	defer a.close()
	acA, _ := a.account(pos[0])
	acB, _ := a.account(pos[1])
	entries, err := transfer.Compare(acA, acB)
	if err != nil {
		return err
	}
	info, err := sessionInfo(ctx, svc, acA.Name, acB.Name)
	if err != nil {
		return err
	}
	var rows []render.DiffRow
	for _, e := range entries {
		if *project != "" && !strings.Contains(strings.ToLower(e.ProjectKey), strings.ToLower(*project)) {
			continue
		}
		if e.State == transfer.Same && !*all {
			continue
		}
		r := render.DiffRow{Entry: e}
		for _, acct := range []string{acA.Name, acB.Name} {
			if s, ok := info[acct+"/"+e.SessionID]; ok {
				if r.Title == "" {
					r.Title = s.Title
				}
				if s.Last.After(r.Last) {
					r.Last = s.Last
				}
				r.Project = s.Project
				r.Live = r.Live || s.Live
			}
		}
		rows = append(rows, r)
	}
	if c.json {
		return a.printJSON(rows)
	}
	a.term(c).Diff(acA.Name, acB.Name, rows, len(entries))
	return nil
}

func cmdTransfer(ctx context.Context, a *app, args []string, mode transfer.Mode) error {
	name := map[transfer.Mode]string{transfer.Copy: "copy", transfer.Move: "move"}[mode]
	var c common
	fs := newFlags(name, a, &c)
	to := fs.String("to", "", "target account (required)")
	from := fs.String("from", "", "source account (required for a batch; found automatically for named sessions)")
	project := fs.String("project", "", "batch: only projects whose folder name contains this")
	include := fs.String("include", "missing,ahead", "batch: which sessions, any of missing (only in source), ahead (source is longer)")
	days := fs.Int("days", 0, "batch: only sessions active in the last N days (0 = any)")
	apply := fs.Bool("apply", false, "carry out the plan (without it nothing is written)")
	ids, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *to == "" {
		return fmt.Errorf("usage: ccli %s <session>... --to <account> [--apply]\n       ccli %s --from <account> --to <account> [--project p] [--apply]", name, name)
	}
	svc, err := a.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	defer a.close()
	target, err := a.account(*to)
	if err != nil {
		return err
	}
	opt := transfer.Options{
		// Read afresh for every plan and apply: a session may start while a
		// batch runs.
		Live: func(id string) bool {
			for _, l := range account.LiveSessions(a.accts) {
				if l.SessionID == id {
					return true
				}
			}
			return false
		},
		ArchiveDir: filepath.Join(store.DataDir(), "archive"),
	}

	type pick struct {
		id     string
		source account.Account
	}
	var picks []pick
	if len(ids) > 0 {
		for _, ref := range ids {
			acct, id, err := svc.Resolve(ctx, ref)
			if err != nil {
				return err
			}
			src := acct
			if *from != "" {
				src = *from
			} else if acct == target.Name {
				// The newest copy is already in the target; look for it elsewhere.
				src = ""
				for _, ac := range a.accts {
					if ac.Name == target.Name {
						continue
					}
					if _, err := transfer.Locate(ac, id); err == nil {
						if src != "" {
							return fmt.Errorf("%s is in several accounts; name the source with --from", query.ShortID(id))
						}
						src = ac.Name
					}
				}
				if src == "" {
					return fmt.Errorf("%s is only in %s", query.ShortID(id), target.Name)
				}
			}
			ac, err := a.account(src)
			if err != nil {
				return err
			}
			picks = append(picks, pick{id, ac})
		}
	} else {
		if *from == "" {
			return errors.New("name sessions, or give --from for a batch")
		}
		src, err := a.account(*from)
		if err != nil {
			return err
		}
		entries, err := transfer.Compare(src, target)
		if err != nil {
			return err
		}
		want := map[transfer.State]bool{}
		for _, w := range strings.Split(*include, ",") {
			switch strings.TrimSpace(w) {
			case "missing":
				want[transfer.OnlyA] = true
			case "ahead":
				want[transfer.AAhead] = true
			case "":
			default:
				return fmt.Errorf("--include: unknown value %q (use missing, ahead)", w)
			}
		}
		info, err := sessionInfo(ctx, svc, src.Name)
		if err != nil {
			return err
		}
		cutoff := time.Time{}
		if *days > 0 {
			cutoff = time.Now().AddDate(0, 0, -*days)
		}
		for _, e := range entries {
			if !want[e.State] {
				continue
			}
			if *project != "" && !strings.Contains(strings.ToLower(e.ProjectKey), strings.ToLower(*project)) {
				continue
			}
			if !cutoff.IsZero() && info[src.Name+"/"+e.SessionID].Last.Before(cutoff) {
				continue
			}
			picks = append(picks, pick{e.SessionID, src})
		}
	}
	if len(picks) == 0 {
		fmt.Fprintln(a.out, "Nothing to "+name+".")
		return nil
	}
	sort.SliceStable(picks, func(i, j int) bool { return picks[i].id < picks[j].id })
	// The same session named twice (two prefixes of one id) is done once.
	uniq := picks[:0]
	for i, p := range picks {
		if i == 0 || p.id != picks[i-1].id || p.source.Name != picks[i-1].source.Name {
			uniq = append(uniq, p)
		}
	}
	picks = uniq

	var ops []transfer.Op
	for _, p := range picks {
		op, err := transfer.Plan(p.id, p.source, target, mode, opt)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	info, err := sessionInfo(ctx, svc)
	if err != nil {
		return err
	}
	t := a.term(c)
	if !*apply {
		if c.json {
			return a.printJSON(ops)
		}
		t.Plan(name, ops, info, opt.ArchiveDir)
		return nil
	}
	var results []transfer.Result
	var failed int
	for _, op := range ops {
		if op.Action == transfer.Refuse {
			results = append(results, transfer.Result{Op: op})
			continue
		}
		res, err := transfer.Apply(op, opt)
		if err != nil {
			failed++
			res.Op = op
			res.Op.Reason = err.Error()
			res.Op.Action = transfer.Refuse
		}
		results = append(results, res)
	}
	if _, err := a.refresh(ctx, false); err != nil {
		return err
	}
	if c.json {
		return a.printJSON(results)
	}
	t.Results(name, results, info)
	if failed > 0 {
		return fmt.Errorf("%d of %d sessions failed", failed, len(ops))
	}
	return nil
}
