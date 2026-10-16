package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lr00rl/ccli/internal/account"
	"github.com/lr00rl/ccli/internal/quota"
	"github.com/lr00rl/ccli/internal/render"
	"github.com/lr00rl/ccli/internal/store"
)

func init() {
	commands["quota"] = cmdQuota
	commands["statusline"] = cmdStatusline
}

func cmdQuota(ctx context.Context, a *app, args []string) error {
	var c common
	var accts accountsFlag
	fs := newFlags("quota", a, &c)
	fs.Var(&accts, "a", "account (repeat or comma separate; default all)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := a.prepare(ctx, c, accts)
	if err != nil {
		return err
	}
	defer a.close()
	snaps, err := quota.Load(store.DataDir())
	if err != nil {
		return err
	}
	now := time.Now()
	var rows []render.QuotaRow
	for _, ac := range a.accts {
		if len(accts) > 0 && !contains(accts, ac.Name) {
			continue
		}
		r := render.QuotaRow{Account: ac.Name, Email: ac.Email}
		if s, ok := snaps[ac.Name]; ok {
			r.Snapshot = &s
		}
		if r.Last5h, r.Cost5h, err = svc.Usage(ctx, ac.Name, now.Add(-5*time.Hour)); err != nil {
			return err
		}
		if r.Last7d, r.Cost7d, err = svc.Usage(ctx, ac.Name, now.Add(-7*24*time.Hour)); err != nil {
			return err
		}
		rows = append(rows, r)
	}
	if c.json {
		return a.printJSON(rows)
	}
	a.term(c).Quota(rows)
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

const statuslineHelp = `Usage: ccli statusline -- <command> [args]

Wraps your status line command. Claude Code passes the status line a JSON
object on stdin that includes your subscription's rate limits; ccli records
them for "ccli quota", then runs <command> with the same input and prints its
output unchanged. ccli never edits settings.json. To use it, change your
statusLine command, for example:

  "statusLine": {"type": "command", "command": "ccli statusline -- ~/.claude/ccline/ccline"}
`

// cmdStatusline is on the status line's hot path: no index, no database,
// no identity lookups.
func cmdStatusline(_ context.Context, a *app, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(a.out, statuslineHelp)
		return nil
	}
	accts, _ := account.DiscoverDirs()
	code := quota.Wrap(args, os.Stdin, a.out, a.errw, store.DataDir(), accts, os.Getenv("CLAUDE_CONFIG_DIR"), time.Now())
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// exitCode carries a wrapped command's exit status out of Main.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
