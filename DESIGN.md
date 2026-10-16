# ccli design

`ccli` reports on Claude Code sessions across several local accounts and moves
sessions between them. An account is one Claude Code config dir: `~/.claude`
(account `default`) or `~/.claude-<name>` (account `<name>`), selected with
`CLAUDE_CONFIG_DIR`.

## Boundaries

ccli is a reader. The only things it writes are:

- its own database, `$XDG_DATA_HOME/ccli/ccli.db` (default
  `~/.local/share/ccli/ccli.db`, override `CCLI_DB`);
- for `copy` and `move` only: transcript files of the copied session in the
  target account (`projects/<key>/<id>.jsonl`, `projects/<key>/<id>/`,
  `file-history/<id>/`), and for `move`, an archive of the source under
  `$XDG_DATA_HOME/ccli/archive/` before the source files are removed.

It never reads or writes credentials (Keychain, `.credentials.json`), never
writes `.claude.json`, `settings*.json`, `sessions/` (the live-process
registry), `teams/`, `jobs/`, `session-env/`, or anything else in a config dir.
From `.claude.json` it reads only `oauthAccount.emailAddress`,
`organizationName` and `billingType`, to label an account. It never sends a
request to a model. Quota is read through the official `claude` binary only
(see Quota).

## What a session consists of

Paths are relative to the account's config dir.

| Path | Content | Indexed | Copied |
| --- | --- | --- | --- |
| `projects/<key>/<id>.jsonl` | main transcript, append-only | yes | yes |
| `projects/<key>/<id>/subagents/agent-<a>.jsonl` | subagent transcript | yes | yes |
| `projects/<key>/<id>/subagents/agent-<a>.meta.json` | `agentType`, `description`, `toolUseId`, `spawnDepth`, `requestShape` | yes | yes |
| `projects/<key>/<id>/subagents/workflows/<run>/agent-<a>.jsonl` | workflow agent transcript | yes | yes |
| `projects/<key>/<id>/subagents/workflows/<run>/journal.jsonl` | workflow events (launched, started, result, failed) | yes | yes |
| `projects/<key>/<id>/tool-results/`, `scratchpad/` | large tool outputs, scratch files | no | yes |
| `file-history/<id>/` | `/rewind` checkpoints | no | yes |
| `projects/<key>/memory/` | project auto memory, shared by all sessions | no | no |
| `sessions/<pid>.json` | live process: `sessionId`, `status`, `cwd` | read for liveness | no |

## Transcript fields used

- Every line: `type`, `timestamp`, `cwd`, `sessionId`, `isSidechain`, `isMeta`.
- `assistant`: `message.id`, `requestId`, `message.model`, `message.usage`
  (`input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`, `cache_creation.ephemeral_5m_input_tokens`,
  `cache_creation.ephemeral_1h_input_tokens`, `server_tool_use.web_search_requests`,
  `speed`), `message.content[].type/name/id/input.skill`.
- `user`: `message.content` (string or blocks), `promptSource`, `origin.kind`.
  A prompt is a user line that is not meta, not a sidechain, carries text and
  no `tool_result`. `promptSource == "typed"` or `origin.kind == "human"`
  marks it as typed by a person; task notifications, scheduled and SDK
  prompts are counted separately.
- Titles: `custom-title.customTitle` (from `/rename`), then
  `ai-title.aiTitle`, then the first typed prompt.
- Team membership: `teamName`, `agentName` on lines; `agent-name` lines.
- Links: `continued-in.continuedInSessionId`, `bridgeSessionId`.
- `cost-state.totalCostUSD`: Claude Code's own estimate, kept for comparison.

One API response is written as several lines that share `message.id` and carry
identical usage (one line per content block). Usage is counted once per
`message.id` (falling back to `requestId`). A later line with different usage
for the same id replaces the earlier one. Across files, the first file that
recorded a response owns it, so a forked or copied session is not counted twice.
Model `<synthetic>` is skipped.

## Relationships

ccli builds a session graph from these sources:

| Relation | Source |
| --- | --- |
| session -> subagent | the subagent folder; `meta.toolUseId` names the parent's tool call, `spawnDepth` the nesting |
| session -> workflow run -> agents | `subagents/workflows/<run>/`, its `journal.jsonl` |
| session -> continued session | `continued-in` line |
| team lead -> teammates | teammate transcripts' `teamName` / `agentName`; a team is named `session-<first 8 chars of the lead's session id>`, and `teams/<team>/config.json` (`leadSessionId`) confirms it while the team exists (Claude Code removes it at session end) |
| fork / copy | sessions sharing response ids; the earliest owner is the origin |

Background jobs (`jobs/<id>/state.json`) are not read: the docs call that file
unstable and point to `claude agents --json` instead, which is left for later.

`ccli show <id>` prints the tree. The graph is stored as edges so `ccli web`
can draw it later.

## Store

SQLite through `modernc.org/sqlite` (pure Go). Aggregates are kept per file,
so one file can be re-parsed without touching others.

- `files`: path, account, session, kind (main, subagent, workflow), agent id,
  size, mtime, parsed offset, head hash, last timestamp, missing flag, and the
  per-file aggregates (prompts by source, replies, tool calls, active ms,
  first/last timestamp, cwd, titles, team, agent name, cost-state).
- `responses`: one row per deduplicated API response with tokens by kind,
  model, speed, timestamp, owning file, and cost at index time.
- `activity`: active milliseconds per file per UTC hour.
- `tools`: tool and skill use counts per file.
- `links`: continuation links (`continued-in`) per file.
- `spawns`: the tool-use ids of Agent calls per file, to find which subagent
  spawned which.
- `processes`: each file's latest `cost-state` snapshot per process and model.
- `runs`: workflow run event counts from `journal.jsonl`.
- `dirty`: sessions whose reconciliation is out of date.
- `meta`: schema version, price table hash, reconciliation version.

Rows are never deleted when Claude Code deletes a transcript: the file is
marked missing and its history stays. A database with another schema version
is renamed aside, never dropped.

## Indexing

Every command first brings the index up to date (`--no-index` skips it).

- A file whose size grew is read from its saved offset after the first 4 KiB
  hash matches. A changed hash or a smaller size means a rewrite: the file's
  rows are dropped and it is parsed again from the start.
- Only complete lines are consumed; a half-written last line waits for the
  next run.
- Active time is the sum of gaps under 5 minutes between consecutive lines of
  the main transcript, credited to the hour of the later line. The last
  timestamp is saved so the next run continues the sum.
- Files are parsed by a worker pool; one goroutine writes to SQLite.
- Write transactions take the database lock when they begin, and an update
  applies only if the file's stored offset still equals the offset its parse
  started from, so two ccli runs never add the same bytes twice.
- A file that cannot be read keeps its offset, is counted in the run's
  errors, and is retried next run; it does not stop the run.
- The creation time of a file (macOS birth time, Linux `statx`) decides which
  of two files holding the same response owns it; without one, the file's
  first line time decides. The time stored for a file only moves earlier, so
  a file replaced by a copy written back into it keeps its place.

### Reconciliation

Each applied update marks its session dirty in the same transaction, and every
run ends by reconciling the dirty sessions and clearing the marks, so a run
that stops early leaves the work to the next one. A price table change marks
every session.

For each process in a dirty session, the latest `cost-state` snapshot is
compared per model with the responses recorded between the process's start
and end. Where the snapshot shows more tokens, one `adjustment` response
carries the missing tokens, priced from the table at base rates, so a price
override applies to them as to every recorded call. Nothing is subtracted,
Claude Code's own cost figure is not used, and an unpriced model's adjustment
carries no cost.

A copied or forked transcript repeats its original's `cost-state` lines, so
one process can appear in several sessions. It is reconciled once, by the
session of the earliest-created file holding it, against the responses of
every session holding it, using the snapshot with the latest end. Sessions
sharing a process with a dirty one are reconciled with it.

## Pricing

An embedded table of list prices per model (USD per million tokens: input,
output, cache read, 5-minute and 1-hour cache writes, fast-mode input and
output), with overrides from `~/.config/ccli/prices.json`. Model IDs are
normalized: a `[1m]` suffix and a date suffix are dropped. A server-side web
search adds $0.01. Unknown models are
counted in tokens and listed as unpriced; their cost is left out, never zero.
Cost is computed at index time; when the table hash changes, every row is
repriced. Subscriptions are not billed per token, so costs are labeled "list
price".

## Query interface

Every command calls `query.Service`; the terminal renderer only formats its
results. `ccli web` will serve the same calls over HTTP.

```go
type Service interface {
    Accounts(ctx context.Context, f Filter) ([]AccountSummary, error)
    Sessions(ctx context.Context, f Filter) (SessionPage, error)
    Session(ctx context.Context, ref string) (*SessionDetail, error)
    Overview(ctx context.Context, f Filter) (*Overview, error)
}
```

Comparing and moving sessions between accounts works on the files themselves,
so it lives in `transfer` (`Compare`, `Plan`, `Apply`); the CLI joins its
results with `Sessions` for titles.

Planned HTTP mapping for `ccli web`:

| Endpoint | Call |
| --- | --- |
| `GET /api/accounts?days=` | `Accounts` |
| `GET /api/sessions?account=&days=&project=&model=&q=&sort=&limit=` | `Sessions` |
| `GET /api/sessions/{id}` | `Session` |
| `GET /api/overview?account=&days=` | `Overview` |
| `GET /api/compare?a=&b=&project=` | `transfer.Compare` |
| `GET /api/quota?account=` | `quota.Reader` |
| `POST /api/transfer` (dry run unless `apply`) | `transfer.Plan` / `transfer.Apply` |

`Filter` is parsed from flags in the CLI and from query parameters on the web.
All result types are plain structs with JSON tags; `--json` prints them as is.

## Commands

```
ccli accounts                      accounts, identity, sessions, 30-day usage, live sessions
ccli sessions [-a acct] [--days N] [--project p] [--model m] [-q text] [--sort last|cost|tokens|active] [-n N]
ccli show <id>                     one session: totals, models, tree of subagents and runs, links, files, resume command
ccli stats [-a acct] [--days N]    overview: KPIs, daily chart, projects, models, hour x weekday, session shape, skills
ccli quota [-a acct]               subscription limits per account
ccli diff <a> <b> [--project p]    sessions only in a, only in b, identical, ahead, diverged
ccli copy <id>... --to acct        copy sessions (fast-forward only)
ccli move <id>... --to acct        copy, then archive and remove the source
ccli copy|move --from a --to b [--project p] [--include missing,ahead] [--days N]   batch
ccli resume <id> [--print]         run claude --resume with the right account and cwd
ccli index [--rebuild]
```

Global flags: `--json`, `--no-index`, `--color auto|always|never`.

## Copy and move

- A session that a live process is running (any account's `sessions/<pid>.json`)
  is refused.
- If the target has no copy, the session is copied. If the target's
  transcript is a byte prefix of the source, it is extended (fast-forward).
  If the target is newer, or the two diverged, the session is refused; resume
  one side with `--fork-session` instead.
- Accounts that share these folders through a symlink are refused, and so is
  a target that holds the session under another project folder.
- The transcript is written to a temporary file and renamed into place.
  Subagent, tool-result, scratchpad and file-history folders are copied file
  by file without overwriting a file that differs. No write passes through a
  symlinked folder in the target. Copies get their own file times, so a copy
  is always newer than its original (the index relies on that).
- `move` moves the source files into `$XDG_DATA_HOME/ccli/archive/<time>/`
  instead of deleting them, and only after every file is verified in the
  target byte for byte; a session folder holding a symlink or another file
  `copy` cannot reproduce is refused. The transcript is archived last, so an
  interrupted move can be run again.
- Usage stays with the account that ran it: after a move the session's past
  cost remains in the source account's totals.
- `copy` and `move` print the plan and change nothing unless `--apply` is
  given, for named sessions and batches alike.
- The project memory folder, the live registry and every config file stay
  untouched.

## Quota

The only documented source of subscription limits is the status line input
(code.claude.com/docs/en/statusline): `rate_limits.five_hour` and
`rate_limits.seven_day`, each with `used_percentage` and `resets_at` (epoch
seconds), for Pro and Max accounts, present after the session's first API
response. `/usage` is not documented as available under `claude -p`, so ccli
does not run it, and it never calls a usage endpoint with stored credentials
(the terms forbid third-party tools collecting or intermediating Claude.ai
credentials).

- `ccli statusline -- <command> [args]` is an opt-in wrapper for the user's
  status line. It reads the JSON from stdin, saves `rate_limits` for the
  account (found from `transcript_path`) to
  `$XDG_DATA_HOME/ccli/quota/<account>.json`, runs the wrapped command with
  the same stdin, and prints its output unchanged. ccli never edits
  `settings.json`; the user changes their `statusLine.command` to use it.
- `ccli quota [-a acct]` shows, per account, the latest snapshot with its age
  and the time to each reset, and next to it the tokens and list-price cost
  ccli counted in the last 5 hours and 7 days. A snapshot whose reset time has
  passed is shown as expired.

## Format stability

Claude Code documents the transcript format as internal and subject to change.
Fields the docs do not name (`agent-*.meta.json`, `continued-in`,
`subagents/workflows/`, `cost-state`) are read when present and ignored when
absent, so a format change degrades a view instead of breaking indexing. The
parser version is stored per file; bumping it re-reads everything.
