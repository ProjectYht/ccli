// Package transcript parses Claude Code session transcripts (JSONL). It is
// pure: it reads bytes and returns what it found, so the indexer can feed it a
// file from any offset and persist the result.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind says which transcript a file is.
type Kind int

const (
	Main     Kind = iota // projects/<key>/<id>.jsonl
	Subagent             // <id>/subagents/agent-*.jsonl
	Workflow             // <id>/subagents/workflows/<run>/agent-*.jsonl
)

func (k Kind) String() string {
	switch k {
	case Subagent:
		return "subagent"
	case Workflow:
		return "workflow"
	}
	return "main"
}

// ActiveGap is the longest pause between two lines that still counts as
// active time.
const ActiveGap = 5 * time.Minute

// MaxLine is the longest line parsed; longer lines are skipped. A variable so
// tests can lower it.
var MaxLine int64 = 64 << 20

// Usage is the token usage of one API response.
type Usage struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	Cache5m   int64 `json:"cache_write_5m"`
	Cache1h   int64 `json:"cache_write_1h"`
	WebSearch int64 `json:"web_search"`
}

// Response is one API response, counted once however many lines carry it.
type Response struct {
	Key   string
	TS    int64 // unix ms
	Model string
	Speed string
	Usage Usage
}

// State carries what the next incremental parse of the same file needs.
type State struct {
	PrevTS int64 // timestamp of the last line seen, for active time
}

// Result is what one parse found.
type Result struct {
	Consumed  int64 // bytes of complete lines read
	Lines     int
	Skipped   int // unparseable or oversized lines
	Responses []Response

	TypedPrompts int // prompts a person typed
	OtherPrompts int // task notifications, scheduled and SDK prompts
	ToolCalls    int
	Tools        map[string]int
	Skills       map[string]int

	ActiveMS int64
	Activity map[int64]int64 // UTC hour (unix ms) -> active ms
	FirstTS  int64
	LastTS   int64
	State    State

	Cwd             string
	CustomTitle     string
	AITitle         string
	Summary         string
	FirstPrompt     string
	TeamName        string
	AgentName       string
	AgentID         string
	BridgeSessionID string
	ContinuedIn     []string
	Spawns          []string // ids of Agent tool calls, which subagent meta files point back to
	Entrypoint      string
	Version         string
	GitBranch       string
	CostUSD         float64
	HasCost         bool
	// Processes holds Claude Code's own per-process totals (cost-state
	// lines), the latest snapshot per process.
	Processes []Process
}

// Process is one Claude Code process's own accounting for a session, from
// its cost-state line. It covers API calls the transcript does not record.
type Process struct {
	Start  int64 // process start, unix ms
	End    int64 // timestamp of the last line before the snapshot
	Models []ModelUsage
}

// ModelUsage is a process's total for one model.
type ModelUsage struct {
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	WebSearch  int64
	CostUSD    float64
}

type line struct {
	Type            string `json:"type"`
	Timestamp       string `json:"timestamp"`
	IsMeta          bool   `json:"isMeta"`
	IsSidechain     bool   `json:"isSidechain"`
	Cwd             string `json:"cwd"`
	RequestID       string `json:"requestId"`
	AITitle         string `json:"aiTitle"`
	CustomTitle     string `json:"customTitle"`
	Summary         string `json:"summary"`
	TeamName        string `json:"teamName"`
	AgentName       string `json:"agentName"`
	AgentID         string `json:"agentId"`
	BridgeSessionID string `json:"bridgeSessionId"`
	ContinuedIn     string `json:"continuedInSessionId"`
	Entrypoint      string `json:"entrypoint"`
	Version         string `json:"version"`
	GitBranch       string `json:"gitBranch"`
	PromptSource    string `json:"promptSource"`
	Origin          *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	TotalCostUSD *float64 `json:"totalCostUSD"`
	StartTime    int64    `json:"startTime"`
	ModelUsage   map[string]struct {
		Input      int64   `json:"inputTokens"`
		Output     int64   `json:"outputTokens"`
		CacheRead  int64   `json:"cacheReadInputTokens"`
		CacheWrite int64   `json:"cacheCreationInputTokens"`
		WebSearch  int64   `json:"webSearchRequests"`
		CostUSD    float64 `json:"costUSD"`
	} `json:"modelUsage"`
	Message json.RawMessage `json:"message"`
}

type assistantMsg struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		Split         *struct {
			M5 int64 `json:"ephemeral_5m_input_tokens"`
			H1 int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		Server *struct {
			WebSearch int64 `json:"web_search_requests"`
		} `json:"server_tool_use"`
		Speed string `json:"speed"`
	} `json:"usage"`
	Content []block `json:"content"`
}

type block struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

type userMsg struct {
	Content json.RawMessage `json:"content"`
}

// Parse reads complete lines from r. kind decides what counts toward prompts
// and active time; st continues an earlier parse of the same file.
func Parse(r io.Reader, kind Kind, st State) (Result, error) {
	res := Result{
		Tools:    map[string]int{},
		Skills:   map[string]int{},
		Activity: map[int64]int64{},
		State:    st,
	}
	seen := map[string]int{}    // response key -> index in res.Responses
	blocks := map[string]bool{} // tool_use ids already counted
	continued := map[string]bool{}
	br := bufio.NewReaderSize(r, 1<<20)
	var buf []byte     // a line longer than the reader's buffer, being assembled
	var pending int64  // bytes of the current line read so far
	oversized := false // the current line is longer than MaxLine
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			pending += int64(len(chunk))
			if !oversized {
				if pending > MaxLine {
					oversized, buf = true, buf[:0]
				} else {
					buf = append(buf, chunk...)
				}
			}
			continue
		}
		if err == io.EOF {
			break // a partial last line waits for the next parse
		}
		if err != nil {
			return res, err
		}
		pending += int64(len(chunk))
		res.Consumed += pending
		res.Lines++
		if oversized || pending > MaxLine {
			res.Skipped++
		} else {
			ln := chunk
			if len(buf) > 0 {
				buf = append(buf, chunk...)
				ln = buf
			}
			handleLine(bytes.TrimSpace(ln), kind, &res, seen, blocks, continued)
		}
		buf, pending, oversized = buf[:0], 0, false
	}
	return res, nil
}

func handleLine(raw []byte, kind Kind, res *Result, seen map[string]int, blocks, continued map[string]bool) {
	if len(raw) == 0 {
		return
	}
	var l line
	if json.Unmarshal(raw, &l) != nil {
		res.Skipped++
		return
	}
	ts := parseTS(l.Timestamp)
	if ts > 0 {
		if res.FirstTS == 0 || ts < res.FirstTS {
			res.FirstTS = ts
		}
		if ts > res.LastTS {
			res.LastTS = ts
		}
		if kind == Main {
			if prev := res.State.PrevTS; prev > 0 && ts > prev {
				if gap := ts - prev; gap < ActiveGap.Milliseconds() {
					res.ActiveMS += gap
					res.Activity[ts-ts%3_600_000] += gap
				}
			}
			if ts > res.State.PrevTS {
				res.State.PrevTS = ts
			}
		}
	}
	if res.Cwd == "" && l.Cwd != "" {
		res.Cwd = l.Cwd
	}
	if l.TeamName != "" {
		res.TeamName = l.TeamName
	}
	if l.AgentName != "" {
		res.AgentName = l.AgentName
	}
	if l.AgentID != "" && res.AgentID == "" {
		res.AgentID = l.AgentID
	}
	if l.BridgeSessionID != "" {
		res.BridgeSessionID = l.BridgeSessionID
	}
	if l.Entrypoint != "" {
		res.Entrypoint = l.Entrypoint
	}
	if l.Version != "" {
		res.Version = l.Version
	}
	if l.GitBranch != "" {
		res.GitBranch = l.GitBranch
	}

	switch l.Type {
	case "assistant":
		assistant(l, ts, res, seen, blocks)
	case "user":
		if kind == Main && !l.IsMeta && !l.IsSidechain {
			user(l, res)
		}
	case "custom-title":
		if l.CustomTitle != "" {
			res.CustomTitle = clean(l.CustomTitle)
		}
	case "ai-title":
		if l.AITitle != "" {
			res.AITitle = clean(l.AITitle)
		}
	case "summary":
		if l.Summary != "" {
			res.Summary = clean(l.Summary)
		}
	case "continued-in":
		if l.ContinuedIn != "" && !continued[l.ContinuedIn] {
			continued[l.ContinuedIn] = true
			res.ContinuedIn = append(res.ContinuedIn, l.ContinuedIn)
		}
	case "cost-state":
		if l.TotalCostUSD != nil {
			res.CostUSD, res.HasCost = *l.TotalCostUSD, true
		}
		if l.StartTime > 0 && len(l.ModelUsage) > 0 {
			p := Process{Start: l.StartTime, End: res.State.PrevTS}
			if p.End == 0 {
				p.End = res.LastTS
			}
			for m, u := range l.ModelUsage {
				p.Models = append(p.Models, ModelUsage{Model: m, Input: u.Input, Output: u.Output,
					CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, WebSearch: u.WebSearch, CostUSD: u.CostUSD})
			}
			sort.Slice(p.Models, func(i, j int) bool { return p.Models[i].Model < p.Models[j].Model })
			replaced := false
			for i := range res.Processes {
				if res.Processes[i].Start == p.Start {
					res.Processes[i], replaced = p, true
				}
			}
			if !replaced {
				res.Processes = append(res.Processes, p)
			}
		}
	}
}

func assistant(l line, ts int64, res *Result, seen map[string]int, blocks map[string]bool) {
	var m assistantMsg
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID != "" {
			if blocks[b.ID] {
				continue
			}
			blocks[b.ID] = true
		}
		res.ToolCalls++
		res.Tools[b.Name]++
		if (b.Name == "Agent" || b.Name == "Task") && b.ID != "" {
			res.Spawns = append(res.Spawns, b.ID)
		}
		if b.Name == "Skill" && len(b.Input) > 0 {
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Skill != "" {
				res.Skills[in.Skill]++
			}
		}
	}
	if m.Usage == nil || m.Model == "<synthetic>" {
		return
	}
	key := m.ID
	if key == "" {
		key = l.RequestID
	}
	if key == "" {
		return
	}
	u := Usage{Input: m.Usage.Input, Output: m.Usage.Output, CacheRead: m.Usage.CacheRead}
	if s := m.Usage.Split; s != nil && s.M5+s.H1 > 0 {
		u.Cache5m, u.Cache1h = s.M5, s.H1
	} else {
		u.Cache5m = m.Usage.CacheCreation
	}
	if m.Usage.Server != nil {
		u.WebSearch = m.Usage.Server.WebSearch
	}
	speed := m.Usage.Speed
	if speed == "" {
		speed = "standard"
	}
	r := Response{Key: key, TS: ts, Model: m.Model, Speed: speed, Usage: u}
	if i, ok := seen[key]; ok {
		if res.Responses[i].Usage != u {
			r.TS = res.Responses[i].TS
			res.Responses[i] = r
		}
		return
	}
	seen[key] = len(res.Responses)
	res.Responses = append(res.Responses, r)
}

func user(l line, res *Result) {
	var m userMsg
	if json.Unmarshal(l.Message, &m) != nil || len(m.Content) == 0 {
		return
	}
	text, isPrompt := promptText(m.Content)
	if !isPrompt {
		return
	}
	typed := true
	switch {
	case l.PromptSource != "":
		typed = l.PromptSource == "typed"
	case l.Origin != nil && l.Origin.Kind != "":
		typed = l.Origin.Kind == "human"
	}
	if !typed {
		res.OtherPrompts++
		return
	}
	res.TypedPrompts++
	if res.FirstPrompt == "" {
		if t := strings.TrimSpace(text); t != "" && !strings.HasPrefix(t, "<") {
			res.FirstPrompt = clean(t)
		}
	}
}

// promptText returns the text of a user message and whether it is a prompt
// (text, no tool result).
func promptText(content json.RawMessage) (string, bool) {
	c := bytes.TrimSpace(content)
	if len(c) > 0 && c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) != nil {
			return "", false
		}
		return s, strings.TrimSpace(s) != ""
	}
	var bs []block
	if json.Unmarshal(c, &bs) != nil {
		return "", false
	}
	var text string
	hasText := false
	for _, b := range bs {
		switch b.Type {
		case "tool_result":
			return "", false
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				hasText = true
				if text == "" {
					text = b.Text
				}
			}
		case "image", "document":
			hasText = true
		}
	}
	return text, hasText
}

func parseTS(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// clean collapses whitespace and caps a title at 200 runes.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 200 {
		r := []rune(s)
		s = string(r[:200])
	}
	return s
}
