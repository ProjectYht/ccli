package conv

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Summary describes a tool call in one line: the command, path, pattern or
// target that tells calls of the same tool apart.
func Summary(name string, input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	switch name {
	case "Bash", "Monitor":
		s := firstLine(str("command"))
		if d := str("description"); d != "" && s == "" {
			s = d
		}
		if b, _ := in["run_in_background"].(bool); b {
			s += "  (background)"
		}
		return s
	case "Read":
		s := str("file_path")
		if off, ok := in["offset"].(float64); ok {
			s += fmt.Sprintf(":%d", int(off))
		}
		return s
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		if s := str("file_path"); s != "" {
			return s
		}
		return str("notebook_path")
	case "Grep":
		s := str("pattern")
		if p := str("path"); p != "" {
			s += "  in " + p
		}
		return s
	case "Glob":
		return str("pattern")
	case "Agent", "Task":
		s := str("description")
		if t := str("subagent_type"); t != "" {
			s = t + ": " + s
		}
		if n := str("name"); n != "" {
			s += "  as " + n
		}
		return s
	case "Workflow":
		name := str("name")
		switch {
		case str("script") != "":
			name = WorkflowName(str("script"))
		case str("scriptPath") != "":
			// Saved scripts are named <workflow>-<run id>.js.
			name = strings.TrimSuffix(filepath.Base(str("scriptPath")), ".js")
			if i := strings.Index(name, "-wf_"); i > 0 {
				name = name[:i]
			}
		}
		if r := str("resumeFromRunId"); r != "" {
			name += "  (resumes " + r + ")"
		}
		return name
	case "Skill":
		return strings.TrimSpace(str("skill") + " " + firstLine(str("args")))
	case "WebFetch":
		return str("url")
	case "WebSearch", "ToolSearch":
		return str("query")
	case "SendMessage":
		to := str("to")
		if to == "" {
			to = str("recipient")
		}
		msg := str("summary")
		if msg == "" {
			msg = firstLine(str("message"))
		}
		if msg == "" {
			msg = firstLine(str("content"))
		}
		return strings.TrimSpace("to " + to + ": " + msg)
	case "TodoWrite":
		if todos, ok := in["todos"].([]any); ok {
			return fmt.Sprintf("%d items", len(todos))
		}
	case "TaskStop":
		return str("task_id")
	case "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				s, _ := q["question"].(string)
				return firstLine(s)
			}
		}
	}
	// Anything else, MCP tools included: the first short string argument,
	// preferring the names that usually say what the call is about.
	for _, k := range []string{"url", "query", "path", "file_path", "action", "name", "text", "command", "prompt"} {
		if s := str(k); s != "" {
			return firstLine(s)
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := in[k].(string); ok && s != "" {
			return firstLine(s)
		}
	}
	return ""
}

var metaName = regexp.MustCompile(`name:\s*['"]([^'"]+)['"]`)

// WorkflowName is the name a workflow script declares in its meta block.
func WorkflowName(script string) string {
	if m := metaName.FindStringSubmatch(script); m != nil {
		return m[1]
	}
	return firstLine(script)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

var (
	cmdName = regexp.MustCompile(`<command-name>\s*([^<]*?)\s*</command-name>`)
	cmdArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	tnTag   = func(tag string) *regexp.Regexp { return regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`) }
	tnSum   = tnTag("summary")
	tnRes   = tnTag("result")
	tnStat  = tnTag("status")
	peerRE  = regexp.MustCompile(`(?s)<teammate-message[^>]*?teammate_id="([^"]*)"[^>]*>(.*?)</teammate-message>`)
)

// PromptDisplay turns a prompt as recorded into what a person would
// recognise: "/name args" for a slash command, the summary and result of a
// task notification, the sender and text of a peer message. Other prompts
// are returned unchanged.
func PromptDisplay(text string) string {
	s := strings.TrimSpace(text)
	if m := cmdName.FindStringSubmatch(s); m != nil {
		out := m[1]
		if !strings.HasPrefix(out, "/") {
			out = "/" + out
		}
		if a := cmdArgs.FindStringSubmatch(s); a != nil && strings.TrimSpace(a[1]) != "" {
			out += " " + strings.TrimSpace(a[1])
		}
		return out
	}
	if strings.HasPrefix(s, "<task-notification>") {
		var parts []string
		if m := tnSum.FindStringSubmatch(s); m != nil {
			parts = append(parts, strings.TrimSpace(m[1]))
		} else if m := tnStat.FindStringSubmatch(s); m != nil {
			parts = append(parts, "Task "+strings.TrimSpace(m[1]))
		}
		if m := tnRes.FindStringSubmatch(s); m != nil {
			parts = append(parts, strings.TrimSpace(m[1]))
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n")
		}
		return stripTags(s)
	}
	if m := peerRE.FindStringSubmatch(s); m != nil {
		return "From " + m[1] + ":\n\n" + strings.TrimSpace(m[2])
	}
	if strings.Contains(text, "<pasted_content") {
		return pastes(text)
	}
	return text
}

// Headline is the first line of a prompt as displayed, for lists.
func Headline(text string) string {
	_, text = SplitReminders(text)
	s := headlineText(text)
	if !strings.HasPrefix(s, "[pasted] ") {
		s = PromptDisplay(s)
	}
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(firstNonEmpty(s)), " ")
	return s
}

func firstNonEmpty(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

var (
	reminderRE = regexp.MustCompile(`(?s)^\s*<system-reminder>(.*?)</system-reminder>`)
	commentRE  = regexp.MustCompile(`(?s)^\s*<!--.*?-->`)
)

// SplitReminders separates the <system-reminder> blocks a client put in
// front of a prompt from what the person wrote.
func SplitReminders(text string) (reminders []string, rest string) {
	rest = text
	for {
		if m := reminderRE.FindStringSubmatchIndex(rest); m != nil {
			reminders = append(reminders, strings.TrimSpace(rest[m[2]:m[3]]))
			rest = rest[m[1]:]
			continue
		}
		if m := commentRE.FindStringIndex(rest); m != nil {
			rest = rest[m[1]:]
			continue
		}
		return reminders, strings.TrimLeft(rest, "\n ")
	}
}

var pasteRE = regexp.MustCompile(`(?s)<pasted_content[^>]*>\n?(.*?)\n?</pasted_content[^>]*>`)

// pastes shows pasted blocks as what they are: terminal output as fenced
// text that keeps its lines, prose as a quote that wraps.
func pastes(s string) string {
	return pasteRE.ReplaceAllStringFunc(s, func(m string) string {
		body := pasteRE.FindStringSubmatch(m)[1]
		if !looksLikeOutput(body) {
			var b strings.Builder
			b.WriteString("\n")
			for _, ln := range strings.Split(strings.TrimSpace(body), "\n") {
				if strings.TrimSpace(ln) == "" {
					b.WriteString(">\n")
					continue
				}
				b.WriteString("> " + ln + "  \n") // two spaces: keep the line break
			}
			return b.String() + "\n"
		}
		fence := "```"
		for strings.Contains(body, fence) {
			fence += "`"
		}
		return "\n" + fence + "text\n" + body + "\n" + fence + "\n"
	})
}

// headlineText starts at the first thing in the prompt: what the person
// wrote, or the first line of a paste, marked as such.
func headlineText(s string) string {
	loc := pasteRE.FindStringSubmatchIndex(s)
	if loc == nil || strings.TrimSpace(s[:loc[0]]) != "" {
		return strings.TrimSpace(pasteRE.ReplaceAllString(s, " "))
	}
	if first := firstNonEmpty(s[loc[2]:loc[3]]); first != "" {
		return "[pasted] " + first
	}
	return strings.TrimSpace(pasteRE.ReplaceAllString(s, " "))
}

// looksLikeOutput guesses whether pasted text is terminal output (indented
// or box-drawn lines, prompts, tables) rather than prose.
func looksLikeOutput(body string) bool {
	var lines, code int
	for _, ln := range strings.Split(body, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines++
		if strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t") || strings.ContainsAny(ln, "│├└─╭╰❯x{25B8}") || strings.Contains(strings.TrimSpace(ln), "   ") {
			code++
		}
	}
	return lines > 0 && code*3 >= lines
}
