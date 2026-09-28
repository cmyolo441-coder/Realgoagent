# nova

An autonomous terminal coding agent. Give it a task in a double-line prompt
box and it reads your code, edits files, runs builds and tests, and reports back
what actually happened.

Not a full-screen TUI. It renders in your scrollback, so your normal terminal
workflow — selecting, copying, scrolling, piping — keeps working.

## Build

```bash
go build -o nova ./cmd/nova
./nova
```

## Usage

```bash
# interactive
nova

# start in a specific directory
nova -C ~/src/myproject

# one-shot from a script or CI
nova -p "add a retry wrapper around the HTTP client" -m grok

# first positional words become the prompt too
nova fix the failing test

# continue yesterday's conversation
nova -c

# read-only planning pass
nova --plan
```

### Flags

| Flag | Meaning |
|---|---|
| `-p <text>` | run one prompt, print the result, exit |
| `-m <model>` | model by `provider/model`, bare id, or alias |
| `-C <dir>` | workspace directory |
| `--plan` | read-only tools only |
| `-c` | resume the last session |
| `--theme <name>` | `nova`, `dracula`, `catppuccin`, `gruvbox`, `nord`, `mono` |
| `--no-color` | disable colour |
| `--models` | list configured models |
| `--config <path>` | alternate config file |
| `--max-iterations <n>` | cap the agent loop |
| `-q` | quiet startup |

### Unattended runs

`-p` has no terminal UI. Tools run unattended: file edits the agent exists
to make — writing, editing and patching files — go straight through, and
shell commands run without prompting. Use `--plan` when you want a run that
cannot modify anything at all.

### Models

Preconfigured providers:

| Provider | Models |
|---|---|
| kiosai (default) | `grok-4.7-free`, `muse-spark-1.3-contributor`, `deepseek-v4.1-flash-free`, `space-bunny-alpha`, `longcat-2.5-preview`, `mimo-v2.6-flash` |
| stepfun | `step-5-preview` (1M context) |

Every provider speaks the OpenAI chat-completions wire format. Switch models
mid-session with `/model` or the `-m` flag:

```bash
nova -m step          # alias
nova -m kiosai/grok   # provider/model
```

Add your own by editing `~/.nova/config/config.json`:

```json
{
  "providers": [
    {
      "name": "myprovider",
      "base_url": "https://api.example.com/v1",
      "api_key": "sk-...",
      "models": [{"id": "some-model", "context_window": 128000}]
    }
  ]
}
```

Keys can also come from the environment: `MYPROVIDER_API_KEY`.

## Tools

| Tool | Purpose |
|---|---|
| `read` | numbered file contents with offset/limit |
| `write` | create or replace a file |
| `edit` | exact unique string replacement |
| `multi_edit` | several replacements in one file |
| `bash` | shell commands, with timeout |
| `list_dir` | depth-limited directory tree |
| `glob` | filename patterns, `**/` supported |
| `grep` | regex search with line numbers |
| `patch` | apply a unified diff |
| `todo` | visible task checklist |
| `web_fetch` | fetch a URL as readable text |
| `web_search` | DuckDuckGo Lite results |
| `git_status` / `git_diff` | repository state |
| `task` | delegate a self-contained job to a subagent |

## Interactive commands

```
/help /clear /model /models /theme /themes /mode
/status /config /tools /diff /run /retry /compact /quit
/edits /edits-show /edits-clear /agents /task /roles
```

Type `/` and the command list opens above the prompt box, filtered as you
keep typing. `↑`/`↓` move the highlight, `PgUp`/`PgDn` jump five rows, and
`Home`/`End` go to the ends. `Enter` runs the command when you have typed it
in full, and completes it when you have not — so `/help` runs on one `Enter`,
while `/mod` + `Enter` completes to `/model` and the next `Enter` runs it.
`Tab` only ever completes, never runs.

## Picking a model

`/models`, or `/model` with no argument, opens a picker above the prompt box
listing every model of every enabled provider, with the one in use marked `●`.
`↑`/`↓` move, `PgUp`/`PgDn` jump five rows, `Enter` switches, `Esc` backs out
without changing anything. Switching carries the conversation over, so the new
model keeps its context. `/model <name>` still works for a quick switch —
`/model mimo`, `/model kiosai/grok`.

## The edit viewer

Every file the agent changes is recorded with the text on both sides of the
change, and the change is printed to the transcript the moment it lands:

```
  ✎ internal/tui/keys.go  +14 -3  1.2ms
        31 +  // Overlays take the navigation keys while they are open…
        44 +  // two stacked lists on one screen is unreadable.
        48 -  // the model picker, then the command palette
```

`/edits` opens the list of every change this session, newest last, with a
preview of the highlighted one. `⏎` expands it to the full diff, and
`↑`/`↓` scroll inside it. `/edits-show [path]` prints one diff as a standard
unified diff — the copy-paste form for a bug report — defaulting to the most
recent change. `/edits-clear` forgets the history; the files on disk are
untouched.

`/undo` drops the entry it reverts, so the list never claims a change that is
no longer on disk. Diffs are computed against the file's real contents at the
moment of the edit, so they are accurate even in a directory that is not a git
repository.

## Subagents

Some work does not belong in the main conversation: surveying a subsystem,
chasing a bug across twenty files, running the same investigation five times.
The `task` tool delegates it to a second model that gets its own conversation,
its own tool set and its own turn budget. Only its conclusion comes back; its
intermediate steps never enter the main context.

| Role | Can | Budget |
|---|---|---|
| `explore` | search and read; never changes a file | 40 calls |
| `plan` | investigate and write the plan, without applying it | 30 calls |
| `general` | a self-contained task end to end, edits and commands included | 60 calls |

A subagent's task has to stand alone — it cannot see the conversation it was
delegated from. The model is told this; `/task` is the same thing by hand:

```
/task explore find every caller of Registry.Call and what they pass
/task plan work out how to add a --watch flag without breaking the parser
```

`/agents` lists every subagent the session has run: what role it had, how many
tool calls it made, and what it spent. `/roles` lists the roles.

Subagents are confined to one level. A subagent that tries to delegate further
is refused and told to do the work itself, so one confused instruction cannot
become a tree of agents billed to you. A subagent's tools run with the same
policy as the main agent: unattended, except read-only roles stay read-only.


`Esc` is the way out of whatever you are in, and resolves in this order:

| Situation | `Esc` does |
|---|---|
| the model is working | stops the turn; the reply so far is kept and the turn ends with `· stopped` |
| the model picker is open | closes it, leaving the model as it was |
| the command list is open | closes the list, keeping what you typed |
| the prompt box has text | clears it |
| waiting on a question | type the answer instead |

Stopping a turn leaves the prompt exactly as it was, so the usual reason to
stop is to edit the prompt and send it again.

Other keys: `Enter` sends, `Shift+Enter` inserts a newline, `Tab` completes,
`^C` quits, `^L` clears the display. Scrolling is the terminal's own — the
mouse wheel and `Shift+PgUp` scroll real scrollback, because Nova writes to
the primary screen buffer and leaves mouse reporting off. `PgUp`/`PgDn`
belong to the open overlay while there is one, and do nothing otherwise.

## The prompt box

```
╭─────────────────────── ⠹ working…  (esc to stop) ───────────────╮
│ █                                                                 │
╰───────── nova · kiosai/grok · ~/src/myproject · it 2 ─────────────╯
  esc stop · ^C quit
```

While idle the rails and the hint change to show what is available instead:

```
╭──────────────────────────────────────────────────────── ready ─╮
│ describe what you want built█                                  │
╰───────────────── nova · kiosai/grok · ~/src/myproject ─────────╯
  ⏎ send · ⇧⏎ newline · / for commands · esc clear · ^C quit
```

The rails carry live state and sit *inside* the border, so the box is always
exactly as wide as the terminal. The caret is a solid block, so it stays
visible against any theme or text.

The box is pinned to the bottom of the terminal. On startup Nova pads the
screen so the prompt finishes on the last row instead of trailing the banner;
from then on the box tracks the bottom by itself, because output is always
written directly above it.

Only the bottom of the screen is repainted. Finished output — replies, tool
calls, command output, command results — is printed once and stays in your
scrollback, where you can select and copy it like any other terminal output.
While a turn is in flight the tail of the reply is previewed above the box,
and the box is repainted in place as the spinner turns and you type.

`^L` clears the display but leaves the transcript in scrollback.

## Safety

Tools run without approval prompts. Plan mode is the safety boundary: it
removes write access entirely, leaving read-only tools.

## Layout

```
cmd/nova/          CLI entrypoint and flags
internal/
  agent/           tool-calling loop, event model, retries
  config/          providers, models, persistence
  llm/             streaming OpenAI-compatible client
  md/              markdown renderer + syntax highlighter
  prompt/          system prompt assembly
  session/         save/resume conversations
  theme/           colour palettes
  tools/           tool registry and built-ins
  tui/             terminal driver, double-line prompt, commands
  util/            ANSI-aware width, wrap, truncate
```

## Tests

```bash
go test ./...
go vet ./...
```

## Config locations

| Path | Contents |
|---|---|
| `~/.nova/config/config.json` | providers, keys, settings (mode 0600) |
| `~/.nova/sessions/` | saved conversations |
