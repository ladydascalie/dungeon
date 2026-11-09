# dungeon

A plan that dispatches. One file declares the encounter, the minions, and the
units of work; the tool spawns the minions, pings each one when its turn comes,
runs the acceptance command, and never writes code itself.

**The DM does not play a minion.** A unit that touches files must name a minion
that owns a worktree. Anything else fails validation.

## Status

tmux and dm are the supported backends. herdr and dm both sit behind `mux.go`;
dm is the one dungeon itself is developed in, so it is the reference for agent
status. The tmux-only rail is the exception.

Every command is built: `init`, `check`, `status`, `set`, `view`, `fleet`,
`attach`, `up`, `next`, `land`, `down` and `clear`.

The loop: `dungeon up` spawns one pi lane per minion, `dungeon next` dispatches
what is ready and settles what landed, `dungeon view` shows the graph while it
happens. Nothing else drives it: no daemon, no watcher.

## Use

```sh
dungeon init                       # write a starter .dungeon/plan.yaml
dungeon check                      # reads .dungeon/plan.yaml
dungeon check --json path/plan.yaml

dungeon status                     # who is working, who is not, which pane
dungeon status --json

dungeon set unit ATOM1 working --note "brief written"
dungeon set minion w-atom1 busy

dungeon view                       # the live table, refreshes every 2s
dungeon view --once --width 120    # one frame, for a screenshot or a pipe
dungeon view --detail MOL1         # open the detail pane on one unit
dungeon rail                       # a compact rail of every pane running an agent
dungeon rail toggle                # open, focus or close that rail in this window
dungeon rail restart               # replace the rail here with this binary
dungeon rail restart --all         # replace every rail, in every session

dungeon attach MOL1 /tmp/brief-mol1.md

dungeon link MOL1 https://github.com/org/repo/pull/42

dungeon up                         # spawn the lanes, then one round of next
dungeon up --no-dispatch           # spawn only
dungeon next --dry-run             # say what would go out
dungeon next --timeout 10m

dungeon land                       # merge done units, re-run their accepts
dungeon down                       # close the lanes, keep the run

dungeon clear                      # delete the finished plans, keep the runs
dungeon clear --remove             # delete the state, the briefs and the reports too
dungeon clear --remove-worktrees   # delete the lane worktrees and their branches
dungeon clear --dry-run            # say what would go, touch nothing
```

A dispatch is five steps, and only proof can mark a unit `done`:

1. write `.dungeon/briefs/<unit>.md`: goal, owned paths, worktree, a verdict
   line or an accept command, the reports from its deps, and what the lane may
   not touch;
2. submit it with `herdr agent prompt`;
3. wait for the lane to settle;
4. read `.dungeon/reports/<unit>.md`. No report means `blocked`, never `done`;
5. prove the unit. Run the unit's `accept` command in the lane's worktree:
   exit 0 is `done`, anything else is `rejected`, and no command at all is
   `unproven`. A unit with a `verdict` line skips the command and reads its
   report instead.

A reviewer unit may carry a `verdict` line instead of an `accept` command. Any
report line whose trimmed text equals that verdict marks the unit `done`. A
report without the line marks it `failed` and names the last merge verdict it
saw. A missing report marks it `blocked`. A verdict unit cannot also set
`accept`.

Every brief lists the reports from the unit's deps under `reports from deps`,
one absolute path per dep. The lane does not have to guess where the upstream
report lives.

Every brief also names the dispatching pane, read from `$HERDR_PANE_ID`, and
tells the lane to write its report and then ping that pane with
`herdr pane run <pane>`. The ping wakes the parent agent so it can settle the
unit. It carries no state: the report plus the unit's accept command or verdict
line decide what `done` means.

`up` is idempotent: a minion whose agent is live is left alone, so a second `up`
resumes instead of doubling the fleet. A writer gets its own worktree at
`<repo>/../<encounter>-<minion>` on branch `<encounter>/<minion>`. Lanes run with
`--no-skills --no-mcp` and a tool allowlist that matches their role: `read,bash`
for staff work, `read,bash,edit,write` for writers.

A minion may replace that default with its own `tools` list:
`- {id: scout, role: scout, tools: "read,bash,grep,find,ls"}`. The value is a
comma separated allowlist. An empty value keeps the role default. Names are
lowercase tool names; an empty item, a duplicate or an unknown shape fails
validation.

A reviewer paired with `of: w1` reads `w1`'s worktree, and its pane starts
there, so it reviews the branch that was written from the checkout that holds
it. A staff minion with no pair reads the base checkout. `of` is the writer
whose worktree the reviewer reads.

Worktrunk cuts the worktree when `wt` is on PATH, so its hooks and
ignored-file copies run. dungeon pins the creation path through the
`worktree-path` override, so lanes land in `<repo>.worktrees/`. When a command
needs an existing lane, dungeon asks `wt list --format json`, then
`git worktree list --porcelain`, for the worktree that has the branch checked
out, so a moved worktree is still found. Plain `git worktree add` is the
fallback, used only when `wt` is missing.

When a lane's branch exists but no worktree holds it, `up` attaches the branch
if it has unique commits. A branch with no unique commits is recreated from
base. The brief leads with the branch facts, so the lane sees what it inherited.

`up` records the plan's sha256 in the state. If the plan changes after that,
`status` and the view say `plan changed since dispatch`, and `next` and `land`
refuse to act until you re-run `up` or pass `--force`. The hash is the whole
guard: an edit mid-run is shown to the operator, never silently applied.

The layout follows the plan. One workspace per encounter, labelled with the
encounter name. One tab per role, labelled `worker` or `workers`: singular for
one minion, plural for more, so the tabs say how many lanes are in each. One
pane per minion, split equally from the tab's root pane. A pinned minion stays
in its own pane and is not moved into the workspace.

Panes grow to the right by default, because tall and narrow beats short and
wide. `split: down` in the plan, or `--split down` on `up`, changes it; `left`
and `up` work too.

A minion may name a pane to run in, and `up` uses it when herdr still has it:
`- {id: w1, role: worker, worktree: w1, pane: "w5N:pH"}`. A pane the
operator picked is where their attention already is, so the pin is honoured and
recorded as a hint in the state. If the pane is gone, `up` splits a fresh one
and the note says so. `space f` in the view jumps straight to the selected unit's
lane: a live agent is focused exactly, and with no live agent the recorded
pane's tab is focused instead. That is the shortcut for a `blocked` row.

`land` merges the `done` units into `<encounter>/integration` in declaration
order, aborts on the first conflict, then re-runs each merged unit's accept
against the merged result. It never touches the base branch and never pushes.
`down` closes the lanes and the encounter tab, refuses to stop a working lane
unless you pass `--force`, and leaves the plan, state, briefs and reports. A
pinned pane belongs to the operator, so `down` interrupts its agent and leaves
the shell instead of closing the pane, then re-reads the fleet to check that
the lane really stopped.

`space f` jumps to the selected unit's lane. A blocked row, a reviewer's verdict and a
lane asking for approval are all one keypress away, and the pane it lands on is
the one `up` recorded or the one the plan pinned.

### Agent names

The plan's minion id is the human handle: `minion: w1`, briefs, reports,
worktrees, sessions, state and `dungeon status` all use it. herdr is the one
place that sees a different string.

A run carries a token of exactly two characters from `[0-9a-z]`. It lives as
`token` in `.dungeon/state.json` and is generated on the first `dungeon up`.
Every agent the run starts is named `<minion>-<token>`, so a plan that declares
`w1` appears in herdr as `w1-7f`. The agent name is derived from the state and
the minion: the minion id truncated to 29 characters, a hyphen, then the token,
which keeps it inside herdr's 32 character limit. herdr names are lowercase
only, and the minion id is already restricted to that alphabet.

`up`, `next`, `status`, `view` and `down` all derive the same name from the
state, so a run keeps one identity from spawn to teardown. herdr names are
global: before it starts anything, `up` checks every derived name against
`herdr agent list` and re-rolls the whole token when one is already live, up to
64 draws, then fails with a finding that names the live agent. Two encounters
that both declare `w1` therefore get two agents, `w1-7f` and `w1-k2`, instead of
sharing one.

Truncation is the only way two names in one plan can collide. `dungeon check`
reports it as `E_DUPLICATE_AGENT` and names both ids, so shorten one of them.

A state with no token, written by an older dungeon or by a run already in
flight, keeps using the bare minion id. `up` leaves such a run alone when its
bare agents are already live rather than renaming the fleet under it; the token
arrives on the first `up` that starts a fleet from scratch.

Keys are modal. `space` opens a prefix and a which-key pane bottom right, listing
every action the prefix can take: fold, focus, open link, reload, layout, hide,
fold all, help, quit. Motion, `d`/`enter` and the overlay keys stay direct.
`esc` cancels the prefix, closes an overlay or the legend, and never quits the
app; `ctrl+c` quits. The header says `PREFIX` while the prefix is open, and the
footer says `space actions` otherwise.
`d` or `enter` opens the detail pane for the selected unit: goal, deps, note, history, and
the files kept with it. Two are found by convention,
`.dungeon/briefs/<unit>.md` and `.dungeon/reports/<unit>.md`; anything else comes
from `dungeon attach`. `j`/`k` or `tab` picks a file, `enter` reads it in
place with `j`/`k` scrolling, `esc` goes back. A `.md` brief or report is
rendered as markdown, so the headings, lists and code blocks a lane wrote
survive the trip. Anything else is shown as plain text. A missing brief is shown
as missing, not hidden.

`/` in the reader starts a case-insensitive search over the lines on screen.
Typing jumps to the first match at or after the line the search started on, the
footer counts the hits (`1/7`), `enter` keeps the search, and `n`/`N` step
through the matches, wrapping at either end. `esc` cancels and puts the reader
back where the search began. Every hit is painted, and the one `n` and `N` sit
on takes a second colour. Matches follow the rendered lines, so a phrase that
word wrap splits across two lines will not be found; search a word. A hit is
found in the visible text, never in the colour codes around it.

Markdown is rendered **once**, when the file is opened and again only when
the pane's width changes. Rendering happens at open, never per frame; rendering
it in the frame path made every keystroke pay for a full parse, which is why
scrolling a long report crawled. The style is sparse: reports and briefs keep
colour on headings, links and code, while prose stays plain. It leaves out the
document colour that made the dark style wrap every prose run in an escape
sequence. On a 400 line report that is 45KB and 680 escapes, against 521KB and
64,143 for the dark style, and a render of about 7ms. The reader scrolls at
about 1.3ms a frame.

`link` pins a pull request, a review, or any http(s) url to a unit. The tool
never parses it, so no forge gets special treatment. The row is marked `↗`,
the card shows the url, and `space o` opens it in the browser. `--clear` forgets it.

`view` is the same snapshot as `status`, rendered with charmbracelet. Rows carry
a right-aligned status column (`working w5M:p8`, `waits TOK4`, `needs you`), and
a boxed strip under the list holds everything about the selected unit: goal,
deps, note, and where its lane is.

The two columns are sized to their content and separated by a two column gap.
The left column is as wide as its longest row, the status column as wide as its
longest status, and the pair sits at the left of the frame. On a wide terminal
the block ends where the text ends instead of stretching a goal to reach a status
pinned to the far edge, and every status still lines up. Two layouts, `space t`
and `space s`:

- **tree** (default): dependencies as nesting, drawn with box-drawing
  connectors. Each unit hangs under its deepest dependency, ties going to the
  later one, so every unit appears exactly once and a connector means "this one
  unblocks that one". A fan-in shows `∧N` on
  the joining unit, and the panel lists every input with its status.
- **sections**: urgency-first groups, BLOCKED down to CANCELED.

`space` then `space` folds the selected branch, the same as any other prefixed
action. A foldable row is marked `⛧`, and a folded one adds `[+N]` for how
much it hides. The header counts the folds in force, so a collapsed branch is
never silent. `space c` collapses every branch, `space e` expands everything.
Folds survive a refresh; they are a view of the session, not part of the state
file. `space h` hides done and canceled units and splices their children into
the parent, so a settled chain collapses without leaving orphaned connectors.

The cursor follows a unit across a refresh, not a row. A row shows its state in
the right column, which names it in words and takes the state's colour. The id
and the lane are bold; the goal is faint, so the identifiers and the status
column carry the eye.
`space ?` opens the legend for every key. `j`/`k` move and `g`/`G` jump;
`space r` refreshes and `space q` quits. Piped output prints one frame
instead of opening a TUI.

`dungeon attach <unit> <path>` keeps a file one keystroke from the unit: a
brief, a report, a screenshot, a handoff note. It stores an absolute path, so
the file stays readable wherever the plan is opened from.

Unit statuses: `blocked`, `failed`, `working`, `planned`, `unproven`,
`rejected`, `done`, `canceled`. **`done` means the unit was proved.** A unit
with an `accept` command is done when that command exits 0 in its minion's
worktree. A unit with a `verdict` line is done when its report
carries that line; a report without it is `failed`. No accept and no verdict, no
done: the unit goes `unproven` and its dependents stay blocked, unless the unit
is provisional.

One encounter is one directory: `.dungeon/` holds `plan.yaml`, `state.json`,
`briefs/` and `reports/`. Nothing goes to `$XDG_STATE_HOME`, so a brief survives
until the encounter is archived and `dungeon down` cannot lose it. `dungeon
clear` archives a finished encounter: every unit done or canceled and no lane
live. It deletes the plan only, because the run is the record of what was done;
`--remove` deletes the state, the briefs, the reports and the sessions as well,
and sweeps the run of a plan cleared earlier. `--remove-worktrees` deletes the
lane worktrees and the `<encounter>/<minion>` branches. It is never implied by
`--remove`, because a lane branch can hold work that never landed. A directory
goes only when it is empty, so the default `.dungeon/` keeps its named
encounters. The pane column comes from
`herdr agent list` at read time, matched by the run's derived agent name, so it
cannot go stale and cannot pick up another run's lane.

Exit codes: `0` clean, `1` findings, `2` could not run (missing file, bad usage).

## Plan

See `testdata/plan.yaml`. Roles: `scout` and `reviewer` produce facts and
verdicts; `worker` and `integrator` own a worktree and write.

An integrator unit must not set `accept`: its worktree is cut from base and
does not contain the other writers' work, so the gate can never pass. `dungeon land`
merges and re-gates.

## Loops

A loop declares a review and fix cycle once. `dungeon` expands it into ordinary
units. N rounds expand to 2N+1 units: review-1, fix-1, ..., review-N, fix-N,
review-N+1. Every fixer pass is judged, including the last.

```yaml
loops:
  - id: auth
    reviewer: r1
    fixer: w1
    rounds: 2
    focus: timeout handling and error paths
    pass: "Verdict: PASS"
    paths: [apps/api/**]
    review_goal: Review the auth timeout fix. Cite file:line.
    fix_goal: Fix what the review found. Read its report.
```

That block expands to five units: `auth-review-1`, `auth-fix-1`,
`auth-review-2`, `auth-fix-2` and `auth-review-3`. Each fix depends on its
review. Each review after the first depends on the fix before it.

A loop may declare `deps` for its first review:

```yaml
loops:
  - {id: auth, reviewer: r1, fixer: w1, rounds: 2, deps: [recon],
     focus: timeout handling, pass: "Verdict: PASS", paths: [apps/api/**]}
```

`deps` names declared units, so the loop reviews work that has landed instead of
racing it. Every id must be a declared unit. Without `deps` the first review has
no deps.

`rounds` is 1 to 9. `focus`, `pass` and `paths` are required. `reviewer` must be
a reviewer and `fixer` must be a writer. `paths` are the files the fixer owns.
`review_goal` and `fix_goal` are optional; without them the generated goals name
the round, the focus and the report to read.

A review is proved by its verdict. The pass line marks it `done`. A report
without that line, including a FAIL verdict, marks it `failed`. That failure
releases the next fix.

A passed review cancels its own round's fix and every later planned unit of
that loop. The state note says `loop <id> satisfied at round <n>`.

A generated fix is provisional. It has no accept command, so it settles
`unproven`. That unproven state is not proof: it carries the next review, and
that review is the proof for the fix before it. A provisional unproven unit
releases its dependants; an ordinary unproven unit does not.

A passing review promotes the fix before it from `unproven` to `done`, with the
note `proved by <review>`, so `land` carries that fix's branch. A failed review
promotes nothing.

### Loop display

The tree draws a loop as one summary row, marked `⛧`. The row shows the loop id,
then `⟳ N/cap` and the focus in the goal column, then the loop state in the
status column. A working loop also shows its live pane. Each review hangs under
the summary row in round order, and its fix hangs under the review. The final
review has no fix. A child row shows `review-1` or `fix-1`, with the loop id
prefix stripped, and `review, round N` or `fix, round N` in the goal column.
The full generated goal stays in the unit card.

An unstarted loop is folded to its summary row. A started loop unfolds. An
explicit `space space` fold or unfold wins and survives a refresh, so a loop
that starts while the view is open unfolds itself unless the operator folded it.

`d` or `enter` on a summary row opens a round timeline in the detail pane. It
names the loop, focus, cap, current round and pass line, then one row per round
with the review state and the fix state, plus a final row for the last review. A
failed verdict prints its note under the round, so the merge verdict the reviewer
saw is visible. `j` and `k` move between rounds, `enter` opens the round's live
unit card, `esc` goes back to the timeline, and `space f` jumps to the round's
lane. A second `esc` closes the timeline. `dungeon view --detail <loop id>`
opens it directly.

`dungeon status` prints a LOOPS block after the unit sections, one line per loop:
id, focus, `round N/cap` and state. `status --json` carries the same rows under
`loops`.

## More than one encounter

The default encounter is `.dungeon/plan.yaml`. A second one lives in its own
fold, `.dungeon/<name>/plan.yaml`, so its state, briefs and reports stay
separate. `dungeon init --encounter auth --plan .dungeon/auth/plan.yaml` writes one.

With more than one under `.dungeon`, `dungeon view` opens a picker: `j`/`k`
move, `enter` opens, `q` quits. Every other command refuses to guess, prints the
list and requires `--plan`. With exactly one encounter no flag is needed.
`--once` and piped output refuse too, because a frame cannot answer a prompt.

## The fleet view

`dungeon fleet` shows every encounter under `.dungeon` in one window, newest
touch first. It takes no plan and needs no argument, because it discovers the
encounters itself.

```sh
dungeon fleet                 # live view, refreshes every 2s
dungeon fleet --interval 5s   # a slower tick
dungeon fleet --once          # print one frame and exit
dungeon fleet --json          # print the fleet model
```

Rows sit under BLOCKED, WORKING, READY and SETTLED. A plan that will not read is
named under BLOCKED with its error instead of disappearing. Settled rows fold to
one summary row per encounter. `j`/`k` move, `g`/`G` jump to the ends, `enter`
or `d` opens the detail, and `space` opens the actions: jump to the lane, open
its hunk session, copy the pane id, expand or fold settled units. `q` quits.
`esc` only closes an overlay and never quits, so a detached fleet is never lost
to one keystroke.

The preview under the list reads the selected lane's pane once per refresh tick.
When the pane is gone or unreadable it shows the lane's report instead, from
`.dungeon/reports/<unit>.md`, and the header names which of the two it read.

The fleet view never writes. It reads plans, state, reports and panes. No key
starts a lane, settles a unit, or changes the run.

The tmux case is why it exists. A tmux lane is a detached pane, and dungeon
cannot classify it, so every tmux lane reports status `unknown`. The preview
does not need the agent protocol: it captures the pane, so the last lines of the
lane are visible even when its status is not. `--once` and `--json` are the same
snapshot for a pipe or a script.

## In herdr

A keybinding that opens the view in a right split, from any directory in the
repo. `dungeon` finds the plan by walking up, the way git finds its root:

```toml
[[keys.command]]
key = "prefix+d"
type = "shell"
command = '''pane=$("$HERDR_BIN_PATH" pane split "$HERDR_ACTIVE_PANE_ID" --direction right --ratio 0.4 --focus --cwd "$HERDR_ACTIVE_PANE_CWD" | jq -r '.result.pane.pane_id'); "$HERDR_BIN_PATH" pane run "$pane" dungeon view'''
description = "dungeon: open the encounter view"
```

Change `--direction right` for `down` or `left`. Drop `--focus` if the graph
should open without stealing the cursor.

## In tmux, dm and herdr

dungeon picks a backend from `--mux dm|herdr|tmux`, then `HERDR_ENV=1`, then
`DM_SESSION`, then `TMUX`. With none set, dm is the default. A tmux session is a workspace, a window is a tab, a
pane is a pane. `up` creates the session detached; attach with
`tmux attach -t <encounter>`.

dm is the same shape inside one session: a dm workspace, a dm window (the mux
calls it a tab) and a dm pane. `up` creates the workspace without stealing the
operator's focus, and `FocusAgent` switches workspace, window and pane as
needed. dm answers OSC 7501 and persists the agent name in `state.json`, so
`Agents` reports working, idle, blocked or done instead of `unknown`, and a
restarted dungeon still finds the live lanes. The binary is `dm` on PATH
(`DM_BIN` overrides it), and `DM_SESSION` names the session.

tmux has no agent protocol, so the run looks different in four ways:

- every lane reports status `unknown`, because dungeon cannot tell working from
  idle;
- a unit settles on its report and its accept command, never on lane state, and
  `next` holds an unknown lane until that report lands;
- `down` refuses a lane it cannot classify, so stopping it needs `down --force`;
- a brief's ping line is a `tmux send-keys` command instead of a herdr one.

Agent names, briefs, reports, worktrees, accepts and `land` behave the same on
both. To compare the two backends, run one plan with `--mux herdr` and then
with `--mux tmux`: the difference you see is the lane status herdr reports and
tmux cannot.

### The agent rail

`dungeon rail toggle` opens a pane on the left of the current window running
`dungeon rail`. The rail lists every live pane except itself, including dungeon
lanes in detached sessions that herdr cannot see. `j`/`k` move row by row,
headers included, `enter` switches to the pane and folds a header, `1` to `9`
jump, `r` reloads, and `q` closes the rail. A click on a tile switches to that
pane too, a click on a header selects it, and the mouse wheel moves the
selection. The window holds its place while the selection stays inside it. Agent panes get their own icon and kind; plain terminals appear as
`zsh`, `bash`, or whatever shell is in front. The rail sees a dungeon lane by
its `@dungeon_agent` option, a known agent command, or an OSC report that names
the app; it does not walk the process tree. A title alone is not proof: a stale
`π - ...` title does not keep a shell in the agent list. The rail marks the
pane it runs in with `@agent_rail` before its first read, so it never lists
itself and `rail restart` finds it whatever opened it.

The rail groups its tiles by tmux session, then by encounter inside that
session, and names each group with a header. Panes no encounter owns follow
their session under a `no encounter` header; a session that hosts no encounter
is one group and needs no header. An encounter named after its session is the
same header twice, so it is not printed again.

`z` folds the group under the cursor, `Z` folds the whole session. A folded
group keeps its header with a `▸` mark and its pane count, and `z` again, or
`enter` on the header, unfolds it. Folds survive a refresh, and a number jump
unfolds the groups that hide its pane.

`rail toggle` and `rail restart` hold a lock per window, in `$TMPDIR`, so a
key binding, a hook and a restart that reach the same window at once cannot
leave two rails in it.

The rail is declared into the window's layout, not split into it. A split takes
its columns from the pane beside it, so that pane shrinks and every other pane
keeps its width; opening the rail scales every pane in the window by the same
factor instead, and `dungeon rail restart` puts the new rail where the old one
stood with no pane resized. `rail toggle` reads `#{window_layout}`, writes the
rail in as the first column, and applies the result with `select-layout`. tmux
prints that layout as JSON from 3.4 on; a layout it cannot read falls back to
the plain split.

The rail is one process per window, so a new binary does not climb into the
rails that are already open. Two things cover that: `dungeon rail restart`
replaces the rail in this window, and `--all` walks every `@agent_rail` pane in
every session, reopening each at the width it had. A running rail also checks
the file it runs on every backstop tick and re-execs itself when that file
changes, so `go install` reaches every open rail within one tick, with no
restart and no lost pane.

When a pane's worktree belongs to an encounter, the rail reads that encounter's
state and shows the unit and its status beside the agent. A lane in a worktree
uses the repo's `.dungeon`, not a copy inside the worktree.

The rail also reads OSC 7501, the Program Status Protocol, straight from each
agent pane's raw output. A pane with its own `pipe-pane` keeps it. The rail
starts a `dungeon osc-sink` parser only when the pane has no pipe, and shows the
last root record as the harness state, with its message in the footer. Working
and blocked records die with the process that reported them. `done` and `error`
survive.

The rail wakes on events, not on a clock. One long-lived tmux client in control mode,
on dungeon's own socket, reports topology: window add, close and rename,
layout and pane mode changes, session changes, and clients attaching or
detaching. That client runs `refresh-client -f no-output`, so the pane stream
is deliberately disabled; the rail reads pane state through its `pipe-pane`
sink, never from that stream. fsnotify watches the repo's `.dungeon` tree:
every encounter with its `plan.yaml`, `state.json`, `reports/` and `briefs/`,
plus the OSC record directory, so a plan edit, a state write or a new report
wakes the rail at once. A 10 second backstop, `--backstop`, re-reads in case an
event was missed. A burst coalesces into one re-read 50ms later.

When nothing changes the rail holds one idle tmux connection, two waiting event
channels and one timer, and runs no command per second. Each backstop tick costs
one `tmux list-panes` plus a read of the state files and OSC records it needs.
The tick was one second before, with a process tree walk on every one.

The renderer is bubbletea over a tmux snapshot. A refresh diffs cells instead
of clearing and repainting the pane, which is why it does not flicker. A
launcher only needs to bind `prefix+a` to `dungeon rail toggle`.

## The skill

`skills/dungeon/SKILL.md` teaches a pi agent to run this tool, and its first
rule is the one that matters: the parent orchestrates and does not do the work.
Link it where pi looks for skills:

```sh
ln -s "$PWD/skills/dungeon" ~/.agents/skills/dungeon
```

define fluid success criteria, kill stuck lanes, and supervise at short intervals so a doomloop is caught in a minute, not ten. It
also carries its own recipe for a one-off pane, for the work that does not
deserve an encounter, so the skill stands alone.

