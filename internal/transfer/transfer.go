// Package transfer copies and moves Claude Code sessions between accounts.
// It writes only a session's transcript, sidecar directory, and file-history
// directory. The project memory folder is never copied, moved, or deleted.
package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lr00rl/ccli/internal/account"
)

// Files locates one session's files in one account.
type Files struct {
	Account     account.Account
	SessionID   string
	ProjectKey  string
	Transcript  string // absolute path, "" when absent
	SidecarDir  string // absolute path, "" when absent
	FileHistory string // absolute path, "" when absent
	// SideDirs are the session's folders under other project keys
	// (projects/<other key>/<id>/), absolute paths.
	SideDirs []string
}

// sidecars is every session folder under projects/: the transcript's and
// the side folders.
func (f Files) sidecars() []string {
	var out []string
	if f.SidecarDir != "" {
		out = append(out, f.SidecarDir)
	}
	return append(out, f.SideDirs...)
}

// Import is a byte range of a transcript file that a copy wrote into the
// target. The index leaves usage in it with the account it came from.
type Import struct {
	Path     string `json:"path"`
	From, To int64
}

// State is how two accounts' copies of one session compare.
type State string

const (
	OnlyA    State = "only-a"    // transcript only in a
	OnlyB    State = "only-b"    // transcript only in b
	Same     State = "identical" // byte-identical transcripts
	AAhead   State = "a-ahead"   // b's transcript is a byte prefix of a's
	BAhead   State = "b-ahead"   // a's transcript is a byte prefix of b's
	Diverged State = "diverged"  // neither is a prefix of the other
)

// Entry is one session in a comparison of two accounts.
type Entry struct {
	SessionID  string `json:"session_id"`
	ProjectKey string `json:"project_key"`
	State      State  `json:"state"`
	ASize      int64  `json:"a_size"`
	BSize      int64  `json:"b_size"`
}

var (
	errInvalidID = errors.New("invalid session id")
	errNotFound  = errors.New("session not found")
	errMultiple  = errors.New("session found under more than one project key")
)

// IsNotFound reports whether err is Locate saying the account has no such
// session, as opposed to failing to look.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// sessionIDPattern matches a lowercase session UUID.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const compareBuf = 32 * 1024

// Locate finds the session in the account by scanning projects/*/<id>.jsonl
// (and projects/*/<id>/ when the transcript is absent). It returns an error
// for an invalid id, when the session is not found, or when it is found
// under more than one project key.
func Locate(a account.Account, sessionID string) (Files, error) {
	if !validID(sessionID) {
		return Files{}, fmt.Errorf("%w %q", errInvalidID, sessionID)
	}
	root := filepath.Join(a.Dir, "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Files{}, fmt.Errorf("%w %q", errNotFound, sessionID)
		}
		return Files{}, fmt.Errorf("read %s: %w", root, err)
	}
	// A session's transcript sits under one project folder. When its working
	// folder changed during the session, Claude Code also made side folders
	// (workflow scripts and the like) under the other folders' keys.
	var withTranscript, withDir []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key := e.Name()
		if !validKey(key) {
			continue
		}
		dir := filepath.Join(root, key)
		if isRegular(filepath.Join(dir, sessionID+".jsonl")) {
			withTranscript = append(withTranscript, key)
		}
		if isDir(filepath.Join(dir, sessionID)) {
			withDir = append(withDir, key)
		}
	}
	sort.Strings(withTranscript)
	sort.Strings(withDir)
	var key string
	switch {
	case len(withTranscript) > 1:
		return Files{}, fmt.Errorf("%w: %s (%s)", errMultiple, sessionID, strings.Join(withTranscript, ", "))
	case len(withTranscript) == 1:
		key = withTranscript[0]
	case len(withDir) > 1:
		return Files{}, fmt.Errorf("%w: %s (%s)", errMultiple, sessionID, strings.Join(withDir, ", "))
	case len(withDir) == 1:
		key = withDir[0]
	default:
		return Files{}, fmt.Errorf("%w %q", errNotFound, sessionID)
	}
	out := Files{Account: a, SessionID: sessionID, ProjectKey: key}
	for _, k := range withDir {
		if k != key {
			d, err := filepath.Abs(filepath.Join(root, k, sessionID))
			if err != nil {
				return Files{}, fmt.Errorf("read %s: %w", d, err)
			}
			out.SideDirs = append(out.SideDirs, d)
		}
	}
	transcript := filepath.Join(root, key, sessionID+".jsonl")
	if isRegular(transcript) {
		out.Transcript, err = filepath.Abs(transcript)
		if err != nil {
			return Files{}, fmt.Errorf("read %s: %w", transcript, err)
		}
	}
	sidecar := filepath.Join(root, key, sessionID)
	if isDir(sidecar) {
		out.SidecarDir, err = filepath.Abs(sidecar)
		if err != nil {
			return Files{}, fmt.Errorf("read %s: %w", sidecar, err)
		}
	}
	history := filepath.Join(a.Dir, "file-history", sessionID)
	if isDir(history) {
		out.FileHistory, err = filepath.Abs(history)
		if err != nil {
			return Files{}, fmt.Errorf("read %s: %w", history, err)
		}
	}
	return out, nil
}

// Compare matches main transcripts (projects/<key>/<id>.jsonl with a valid
// id) of two accounts by session id and classifies each. Byte comparison
// streams the files (never reads a whole file into memory) and stops at the
// first difference. Sorted by ProjectKey, then SessionID.
func Compare(a, b account.Account) ([]Entry, error) {
	am, err := listTranscripts(a)
	if err != nil {
		return nil, err
	}
	bm, err := listTranscripts(b)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(am)+len(bm))
	seen := map[string]struct{}{}
	for id := range am {
		ids = append(ids, id)
		seen[id] = struct{}{}
	}
	for id := range bm {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		ae, aok := am[id]
		be, bok := bm[id]
		e := Entry{SessionID: id}
		switch {
		case aok && !bok:
			e.ProjectKey = ae.key
			e.State = OnlyA
			e.ASize = ae.size
		case !aok && bok:
			e.ProjectKey = be.key
			e.State = OnlyB
			e.BSize = be.size
		default:
			// The session is matched by id. When both accounts have it, the
			// entry keeps account a's project key.
			e.ProjectKey = ae.key
			e.ASize = ae.size
			e.BSize = be.size
			st, err := byteState(ae.path, be.path)
			if err != nil {
				return nil, err
			}
			e.State = st
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectKey != out[j].ProjectKey {
			return out[i].ProjectKey < out[j].ProjectKey
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

type transcriptHit struct {
	key  string
	path string
	size int64
}

func listTranscripts(a account.Account) (map[string]transcriptHit, error) {
	root := filepath.Join(a.Dir, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]transcriptHit{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	out := map[string]transcriptHit{}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		key := p.Name()
		if !validKey(key) {
			continue
		}
		dir := filepath.Join(root, key)
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !validID(id) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, name), err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if prev, ok := out[id]; ok {
				return nil, fmt.Errorf("%w: %s (%s, %s)", errMultiple, id, prev.key, key)
			}
			path := filepath.Join(dir, name)
			abs, err := filepath.Abs(path)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			out[id] = transcriptHit{key: key, path: abs, size: info.Size()}
		}
	}
	return out, nil
}

// byteState streams two files and stops at the first difference.
// AAhead means b is a byte prefix of a. BAhead means a is a byte prefix of b.
func byteState(aPath, bPath string) (State, error) {
	af, err := os.Open(aPath)
	if err != nil {
		return "", fmt.Errorf("compare %s: %w", aPath, err)
	}
	defer af.Close()
	bf, err := os.Open(bPath)
	if err != nil {
		return "", fmt.Errorf("compare %s: %w", bPath, err)
	}
	defer bf.Close()
	st, err := relate(af, bf)
	if err != nil {
		return "", fmt.Errorf("compare %s: %w", aPath, err)
	}
	return st, nil
}

func relate(a, b io.Reader) (State, error) {
	bufA := make([]byte, compareBuf)
	bufB := make([]byte, compareBuf)
	for {
		na, eofA, err := readChunk(a, bufA)
		if err != nil {
			return "", err
		}
		nb, eofB, err := readChunk(b, bufB)
		if err != nil {
			return "", err
		}
		n := na
		if nb < n {
			n = nb
		}
		if !bytes.Equal(bufA[:n], bufB[:n]) {
			return Diverged, nil
		}
		if eofA && eofB {
			switch {
			case na == nb:
				return Same, nil
			case na > nb:
				return AAhead, nil
			default:
				return BAhead, nil
			}
		}
		if eofA && na < nb {
			return BAhead, nil
		}
		if eofB && nb < na {
			return AAhead, nil
		}
	}
}

// readChunk fills buf or stops at EOF. A full buffer returns eof false;
// the next call reports EOF when the file ended on that boundary.
func readChunk(r io.Reader, buf []byte) (int, bool, error) {
	n := 0
	for n < len(buf) {
		nn, err := r.Read(buf[n:])
		n += nn
		if err == io.EOF {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
		if nn == 0 {
			return n, false, errors.New("empty read")
		}
	}
	return n, false, nil
}

func validID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// validKey reports whether key can be one path component under projects/.
func validKey(key string) bool {
	if key == "" || key == "." || key == ".." || strings.HasPrefix(key, ".") {
		return false
	}
	return !strings.ContainsAny(key, "/\\\x00")
}

type destPaths struct {
	transcript string
	sidecar    string
	history    string
}

// sessionPaths builds the three destination paths and rejects any that
// would leave accountDir.
func sessionPaths(accountDir, key, id string) (destPaths, error) {
	if !validKey(key) {
		return destPaths{}, fmt.Errorf("invalid project key %q", key)
	}
	if !validID(id) {
		return destPaths{}, fmt.Errorf("%w %q", errInvalidID, id)
	}
	transcript, err := joinWithin(accountDir, filepath.Join("projects", key, id+".jsonl"))
	if err != nil {
		return destPaths{}, err
	}
	sidecar, err := joinWithin(accountDir, filepath.Join("projects", key, id))
	if err != nil {
		return destPaths{}, err
	}
	history, err := joinWithin(accountDir, filepath.Join("file-history", id))
	if err != nil {
		return destPaths{}, err
	}
	return destPaths{transcript: transcript, sidecar: sidecar, history: history}, nil
}

func joinWithin(root, rel string) (string, error) {
	if rel == "" || rel == "." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q escapes %s", rel, root)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes %s", rel, root)
	}
	p := filepath.Join(root, clean)
	if !within(root, p) {
		return "", fmt.Errorf("path %s escapes %s", p, root)
	}
	return p, nil
}

func within(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isMemoryRel(rel string) bool {
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	return len(parts) >= 3 && parts[0] == "projects" && parts[2] == "memory"
}

func isRegular(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}

func isDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}

// sameFile reports whether a and b exist and are the same file or directory,
// through symlinks.
func sameFile(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

func sameDir(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

// Relation compares two transcripts by bytes: AAhead when b is a prefix of
// a, BAhead when a is a prefix of b.
func Relation(a, b string) (State, error) { return byteState(a, b) }
