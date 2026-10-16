// Package account finds the local Claude Code accounts. An account is one
// config dir: ~/.claude ("default") or ~/.claude-<name> ("<name>"). It reads
// only the identity label from .claude.json and the live-process registry; it
// never touches credentials.
package account

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Account is one Claude Code config dir.
type Account struct {
	Name    string `json:"name"`
	Dir     string `json:"dir"`
	Email   string `json:"email,omitempty"`
	Org     string `json:"org,omitempty"`
	Billing string `json:"billing,omitempty"`
}

// Live is a running Claude Code process, from <dir>/sessions/<pid>.json.
type Live struct {
	Account   string `json:"account"`
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Status    string `json:"status,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// NameOf maps a config dir to its account name.
func NameOf(dir string) string {
	b := filepath.Base(filepath.Clean(dir))
	switch {
	case b == ".claude":
		return "default"
	case strings.HasPrefix(b, ".claude-"):
		return strings.TrimPrefix(b, ".claude-")
	}
	return b
}

// Discover lists the accounts. CCLI_CONFIG_DIRS (a list separated by the OS
// path list separator) replaces the default search of ~/.claude and
// ~/.claude-*. A dir counts only if it has a projects/ folder.
func Discover() ([]Account, error) { return discover(true) }

// DiscoverDirs is Discover without reading identities, for hot paths such as
// the status line wrapper.
func DiscoverDirs() ([]Account, error) { return discover(false) }

func discover(withIdentity bool) ([]Account, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var dirs []string
	if v := os.Getenv("CCLI_CONFIG_DIRS"); v != "" {
		dirs = filepath.SplitList(v)
	} else {
		dirs = append(dirs, filepath.Join(home, ".claude"))
		more, _ := filepath.Glob(filepath.Join(home, ".claude-*"))
		dirs = append(dirs, more...)
	}
	seen := map[string]bool{}
	var out []Account
	for _, d := range dirs {
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		if st, err := os.Stat(filepath.Join(d, "projects")); err != nil || !st.IsDir() {
			continue
		}
		a := Account{Name: NameOf(d), Dir: d}
		if withIdentity {
			a.Email, a.Org, a.Billing = identity(d, home)
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Name == "default") != (out[j].Name == "default") {
			return out[i].Name == "default"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Find returns the account with this name.
func Find(accts []Account, name string) (Account, error) {
	for _, a := range accts {
		if a.Name == name {
			return a, nil
		}
	}
	names := make([]string, 0, len(accts))
	for _, a := range accts {
		names = append(names, a.Name)
	}
	return Account{}, errors.New("no account named " + strconv.Quote(name) + " (have: " + strings.Join(names, ", ") + ")")
}

// identity reads the display label of the signed-in account. Claude Code keeps
// .claude.json inside the config dir when CLAUDE_CONFIG_DIR is set, and at
// ~/.claude.json for the default dir.
func identity(dir, home string) (email, org, billing string) {
	paths := []string{filepath.Join(dir, ".claude.json")}
	if filepath.Clean(dir) == filepath.Join(home, ".claude") {
		paths = append(paths, filepath.Join(home, ".claude.json"))
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var v struct {
			OAuthAccount struct {
				EmailAddress     string `json:"emailAddress"`
				OrganizationName string `json:"organizationName"`
				BillingType      string `json:"billingType"`
			} `json:"oauthAccount"`
		}
		err = json.NewDecoder(f).Decode(&v)
		f.Close()
		if err == nil && v.OAuthAccount.EmailAddress != "" {
			return v.OAuthAccount.EmailAddress, v.OAuthAccount.OrganizationName, v.OAuthAccount.BillingType
		}
	}
	return "", "", ""
}

// LiveSessions lists running Claude Code processes across the accounts.
func LiveSessions(accts []Account) []Live {
	var out []Live
	for _, a := range accts {
		files, _ := filepath.Glob(filepath.Join(a.Dir, "sessions", "*.json"))
		for _, f := range files {
			pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
			if err != nil || pid <= 0 || !alive(pid) {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var v struct {
				SessionID string `json:"sessionId"`
				Status    string `json:"status"`
				Cwd       string `json:"cwd"`
			}
			if json.Unmarshal(b, &v) != nil || v.SessionID == "" {
				continue
			}
			out = append(out, Live{Account: a.Name, PID: pid, SessionID: v.SessionID, Status: v.Status, Cwd: v.Cwd})
		}
	}
	return out
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
