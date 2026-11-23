---
name: dungeon
description: 'Default mode for any task that needs decomposition: if it cannot be fixed inline in one pass, it becomes an encounter. A tracked plan of units, one minion per unit, a graph you watch. Orchestrate and delegate with dungeon, the plan-and-dispatch tool. The parent lane writes the plan, spawns minions, reads the graph and lands the result, and never does the work itself. Use when asked to plan, dispatch, monitor or land work with dungeon, when a .dungeon/plan.yaml exists, or when a task should be split across parallel lanes with a mechanical definition of done.'
---

# dungeon: the parent orchestrates

**You do not do the work.** You write the plan, dispatch it, read the graph, and
land the result. A unit is done by a minion in its own worktree, never by you.

If you catch yourself opening a source file to change it, stop. That is a unit
that needs a minion.

This is the whole point of the tool. An orchestrator that implements a unit has
joined the party, and every rule below exists to stop that.

## Default mode

Any task that needs decomposition becomes an encounter. If it cannot be fixed
inline, now, in one pass, it is a plan. A plan keeps the operator's view
and the record of each lane, which the pane path gives up on purpose.

A single second opinion or fresh-context review over an existing diff has
no decomposition, so it stays one pane. See `## One-off delegation` at the end
of this skill. Everything else becomes an encounter, however small the split.

Coalition is not a mode we run. It failed in practice: too many moving parts
for the control it gave. Do not propose it.

## The one rule

A unit that touches files names a minion that writes. You may write exactly two
things: the plan, and your notes. You may not edit a unit's paths, a worktree, or
a branch.

## Orchestrator discipline

- **Decompose into small, self-contained units.** Every piece of work must be split into small units. A unit should be one concern that can be finished in one sitting and reviewed in isolation.
- **Prioritize parallel work.** Identify independent units and dispatch them in parallel. Maximize concurrency while respecting disjoint paths and dependencies.
- **Define fluid success criteria upfront.** Set success criteria before dispatch. If a unit's tests are expected to fail until a later unit cleans up, mark it as provisional or unproven, or use a verdict instead of an accept command. Do not let a lane block on an expected failure.
- **Kill and re-dispatch stuck lanes.** It is entirely fine for the orchestrator to stop a lane that is burning context or looping. Cancel the unit or run `dungeon down`, then re-dispatch the work as a new unit or assign it to a fresh lane. A stuck lane must never stall the encounter.

## Gate before you start

```bash
command -v dungeon          # required
test -n "${DM_SESSION:-}"   # required: dungeon runs in a dm pane
```

Dungeon runs in dm. Its lanes are dm panes in the same session. The dm backend
reads your pane from `$DM_PANE` and the session from `$DM_SESSION`.

What dm gives you:

- Every lane reports OSC 7501 status, so dungeon can tell working from idle.
- A unit settles on its report and its accept command, not on lane state.
- The brief's notify line posts to the session box. The parent reads the box.

## Write the plan

```bash
dungeon init                # writes .dungeon/plan.yaml and .dungeon/.gitignore
```

One unit is one reviewable change: one concern, one minion, one acceptance
command. Units form a DAG through `deps`.

```yaml
encounter: fix-auth-timeouts      # lowercase: it becomes a branch prefix
repo: /abs/path/to/repo
base: main
project: auth                     # dm rail group. Defaults to the repo folder name
concurrency: 3

minions:
  - {id: scout, role: scout, tools: "read,bash,grep,find,ls"}  # facts. no writes
  - {id: w1, role: worker, worktree: w1}            # writes. own worktree
  - {id: r1, role: reviewer, of: w1}                # reads w1. gives a verdict

units:
  - id: recon
    minion: scout
    goal: Where is the read timeout set? Cite file:line.
  - id: api-timeout
    minion: w1
    paths: [apps/api/**]              # required on a worker unit
    deps: [recon]
    goal: Make the read timeout injectable.
    accept: go test ./apps/api/...    # the only path to done
  - id: review-api
    minion: r1
    deps: [api-timeout]
    verdict: "Verdict: PASS"          # the exact report line that proves it
```

A dep names a unit in this plan, or a unit in another plan:

```yaml
  - id: rates-screen
    minion: w2
    paths: [apps/web/**]
    deps:
      - recon
      - {plan: ../../api-core/.dungeon/billing-api/plan.yaml, unit: rates-endpoint}
```

A relative plan path resolves against the directory of the plan that names it.
Keep it relative: an absolute path breaks every other checkout.

`dungeon check` refuses `E_DEP_PLAN` for a plan that is missing, `E_DEP_UNIT`
for a unit that is not there, `E_DEP_SELF` for a dep on its own plan, and a
cycle that crosses plans with `E_CYCLE`.

A cross-plan dep on a writing unit is satisfied only when the other plan's state
says the unit is done and landed. A dep on a unit whose minion does not write, a
reviewer or a scout, is satisfied by done alone: a verdict or a recon report has
no branch, so there is nothing to merge. `land` records
`landed {ref, commit, at}` for a merged unit whose gate passed. A conflict or a
failed gate records nothing, so nothing downstream starts.

The lane gets the other plan's report path under `reports from deps`, and, for a
writing dep, the ref and commit it landed on. That report is the contract: read
the response shape there instead of guessing it.

A dep does not deliver the other repo's code. It orders work and hands over the
contract. A unit that needs the other repo's source still needs its own merge.

`dungeon status` names a cross-plan dep as `plan#unit` under `waits on`.
A dep on a unit the other plan marked `failed` never releases. Read that plan
and re-queue or cancel the unit there.

Loops stay local. `loops[].deps` names declared units in the same plan.

Set `project:` before you dispatch. The encounter workspace joins that group in
dm's rail. With no `project:` it takes the repo folder name, which is usually
right. Set it when the operator's group differs from the folder name: a
`gitops-cluster` checkout under the `gitops` group needs `project: gitops`.

Check the name against the session before dispatch:

```bash
dm list --json | jq -r '.snapshot.all[].project' | sort -u   # groups already in use
```

A lane in the wrong group is a plan bug. dm inherits the active workspace's
project when a request names none, so an untagged plan lands wherever the
operator happened to be. dungeon always names one now, and the plan is where
the name comes from.

Do not add an integrator unit with an `accept` command. Its worktree is cut
from base and does not contain the other writers' work, so the gate can never pass.
`dungeon land` merges the done units and re-runs their accepts against the
merged result. It is the merge gate.

Change a plan with `dungeon edit`, never with a heredoc over the yaml. It edits
one field of one unit or one minion in place, keeps the comments and the blank
lines, prints the diff, and refuses a change that adds a finding:

```bash
dungeon edit unit <id> --paths-add 'apps/api/**'      # widen what it owns
dungeon edit unit <id> --accept 'go test ./apps/api/...'
dungeon edit unit <id> --deps-add <unit>              # repeatable, local
dungeon edit unit <id> --deps-add <plan>#<unit>       # a dep in another plan
dungeon edit unit <id> --goal '...'                   # long text folds itself
dungeon edit unit <id> --verdict ''                   # empty deletes the field
dungeon edit minion <w1> --tools 'read,bash,edit,write'
dungeon edit unit <id> --paths-add 'x/**' --dry-run    # diff, write nothing
```

`dungeon check` must pass before anything runs. A plan that does not validate is
a plan that cannot dispatch.

A minion's `tools` list replaces the role default. The default is `read,bash`
for staff and `read,bash,edit,write` for writers. Leave the field out to keep
the default. List lowercase tool names, comma separated, with no empty items
and no duplicates.

A reviewer with `of: w1` reads `w1`'s worktree, and its pane starts there, so
it reviews the branch that was written from the checkout that holds it. A staff
minion with no pair reads the base checkout. `of` names the writer whose
worktree the reviewer reads and where its pane starts.

A `verdict` line proves a reviewer unit without an accept command. Any report
line whose trimmed text equals the verdict marks the unit `done`. A report
without that line, including a FAIL verdict, marks it `failed`. A missing report
marks it `blocked`. A verdict unit cannot also set `accept`.

Good units, in the order that matters:

- **One concern.** If the goal needs an "and", it is two units.
- **Disjoint paths.** Two units that touch one file are one unit, or they are
  ordered with `deps`.
- **Paths wide enough to pass.** Paths must cover everything the accept
  touches: generated docs, golden test frames, a README row. A worker's
  worktree is cut from base and never contains a dep's work, so a unit that
  builds on a dep's package carries it by copy, and its paths must say so.
  Otherwise `land` hits an add/add conflict between the copy and the dep's
  branch.
- **A real acceptance command.** It runs in the lane's worktree. Exit 0 makes
  the unit done. A reviewer may give a `verdict` line instead, and that exact
  report line makes its unit done. No accept and no verdict means `unproven`,
  which blocks every dependent.
- **An accept at the unit's own indent.** YAML folds a multi-line `goal: >`
  into one string. An `accept:` indented inside that block is goal text, not
  a field, and the unit settles `unproven` with nothing that proves it. Keep
  `accept:` and `verdict:` at the unit's own level.
- **A small unit.** A unit a lane can finish in one sitting gets a report worth
  reading.

Paths are also the commit guard. At settle dungeon diffs the tree against the
commit recorded when the unit was dispatched, so every changed file must sit
inside the unit's paths, whatever carried it: the lane's own commit, a merge, a
fast-forward, a rebase, a cherry-pick or the working tree. A unit that merges a
landed branch therefore declares that branch's files in its own paths. The block
note names the files and the `dungeon edit unit <id> --paths-add '<glob>'` that
widens them.

The guard also refuses a worktree set up to report a tree it cannot see, and the
note names the cause: an assume-unchanged or skip-worktree entry, `core.fileMode`
false, a replace ref, or a clean or smudge filter whose attribute applies to the
tree.

The guard catches drift, not a hostile lane. A lane runs with the same shell in
the same repository, so it can forge git evidence, rewrite the plan or edit the
state. The boundary that holds is the parent reading the diff and the gate, not
a check in this tool.

More than one encounter under `.dungeon`: `dungeon view` opens a picker, and
every other command needs `--plan`, or `--all` on `up`, `next` and `land` to
sweep every encounter. Extra encounters live at `.dungeon/<name>/plan.yaml` with
their own state, briefs and reports. A lane passes the full path; there is no
picker outside the view.

## Loops

A loop: one review and fix cycle, declared once and expanded into units. Use it
when a fix needs more than one pass and the reviewer decides when the work is
good enough.

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

N rounds expand to 2N+1 units: review-1, fix-1, ..., review-N, fix-N,
review-N+1. Every fixer pass is judged, including the last.

That example expands to `auth-review-1`, `auth-fix-1`, `auth-review-2`,
`auth-fix-2` and `auth-review-3`. Each fix depends on its review. Each review
after the first depends on the fix before it.

A loop may declare `deps` for its first review, for example `deps: [recon]`.
The ids must be declared units. The loop then reviews work that has landed
instead of racing it. Without `deps` the first review has no deps. A loop that reviews a writer's work must declare `deps` on that writer's first unit. Without them, round 1 reviews base, finds nothing real, and you cancel it.

`rounds` is 1 to 9. `focus`, `pass` and `paths` are required. `reviewer` must be
a reviewer and `fixer` must be a writer. `paths` are the files the fixer owns.
`review_goal` and `fix_goal` are optional. The default review goal is
adversarial: hostile senior reviewer, edge cases, races, malicious inputs,
leaks, silent failures, one concrete counter-example or repro per finding, and
the pass line only when the findings list is empty. The default fix goal names
the review report to read. A plan overrides either.

A review is proved by its verdict. The pass line marks it `done`. A report
without that line, including a FAIL verdict, marks it `failed`. That failure
releases the next fix.

A passed review cancels its own round's fix and every later planned unit of
that loop. The state note says `loop <id> satisfied at round <n>`.

A generated fix is provisional. It has no accept command, so it settles
`unproven`. That unproven state is not proof: it carries the next review, and
that review judges the fix before it. A provisional unproven unit releases its
dependants. An ordinary unproven unit does not.

A passing review promotes the fix before it from `unproven` to `done`, with the
note `proved by <review>`, so `land` carries that fix's branch. A failed review
promotes nothing.

In the view, a loop is one summary row marked `⛧`: the loop id, `⟳ N/cap` and
the focus, then the loop state and live pane. Each review hangs under it in
round order, and its fix hangs under the review. The final review has no fix. A
child row shows `review-1` or `fix-1`, and `review, round N` or `fix, round N`
as its goal. An unstarted loop is folded. A started loop unfolds. An explicit
fold or unfold wins and survives a refresh.

`d` or `enter` on a loop summary row opens the round timeline: loop, focus, cap,
current round, pass line, and one row per round with the review and fix state. A
failed verdict prints its note. `j` and `k` move between rounds, `enter` opens
the round's live unit card, `esc` goes back to the timeline, and `space f` jumps
to the round's lane. `dungeon status` ends with a LOOPS block naming each loop
and its state.

## The loop

```bash
dungeon up            # spawn a lane per minion, then one round of next
dungeon next          # settle what landed, dispatch what is ready, wait
dungeon status        # who is working, who is not, which pane
dungeon view          # live graph. d detail, space actions. picker when >1
```

Run `dungeon next` until it says nothing to do. It is idempotent and safe in a
loop, and there is no daemon to keep alive.

Between rounds, watch the box at short intervals. See `## Supervise at short
intervals` below. A long block is time a looping lane spends burning context.

`next` runs one accept command at a time, inline and silent, up to
`--accept-timeout` (10m by default). Do not abort it. A killed accept proves
nothing and the unit stays `working`. Run `next` again instead.

`up` builds one workspace per encounter, one window per role and one pane per
minion, so the layout reads like the plan. The workspace lives in the current
dm session. Panes grow to the right unless the plan says `split: down`, and
they are equal shares of the window.

`dungeon up` is idempotent too. A minion whose agent is live is left alone, so a
second `up` resumes the fleet instead of doubling it.

## The fleet loop

A fleet of encounters is one sweep, not N blocking calls. Do not run one
blocking `up` or `next` per encounter.

```bash
dungeon up --all      # spawn every encounter, then one next --all round
dungeon next --all    # settle and dispatch every encounter, one mux, one deadline
dungeon fleet         # watch every encounter in one window
```

Run `dungeon next --all` until it says nothing to do. Keep `dungeon fleet` open
and watch the `lane/` prefix at short intervals between rounds.

A parent must not fan blocking `dungeon up` or `dungeon next` calls, one per
encounter, out of its tool layer. The parent is blind for the whole sweep and
cannot answer a blocked lane until the last call returns. One `--all` call
drives every encounter in one loop and its digest names every blocked lane.

One `next --all` call's `--timeout` covers the whole sweep, not each plan. The
watch discipline still applies between rounds: set it short, then call again.

A plan that does not validate is skipped and named as
`skipped <path>: <first finding>`. The sweep runs over the rest. No target left
after filtering is exit 2.

A sweep that left a plan out exits 1 and says `N of M encounters skipped`, so a
scripted dispatch cannot lose an encounter in silence.

`--dry-run` predicts one pass per plan: `would dispatch`, `would settle` and
`would block` sit inside each plan's block. It settles no lane, runs no accept
and writes no state. The brief is the one file it still writes, as `next`
always has. Single-plan `next --dry-run` behaves the same way.

Each plan keeps its own concurrency and its own state. A state that moved is
saved as its plan's pass ends, so a crash loses nothing.

`--all` with `--plan` or `--state` is exit 2: a sweep keeps one state per
encounter. With one encounter no flag is needed.

`--only` names the encounters a sweep means, once or comma separated, by the
plan's `encounter:` field else the directory holding `plan.yaml`. A name that
matches no encounter, or a plan that failed to load, is exit 2 with that plan's
finding; no target after filtering is exit 2; `--only` without `--all` is exit
2. Every encounter is still loaded, so a broken plan out of scope cannot fail a
run nobody asked for. It exists because `land --all` in a repo with finished
encounters lands all of them, re-runs their gates, and one stale conflict
withholds the combined branch for the live work, so a sweep must be able to say
`--only a,b`.

`land --all` merges the fleet's integration branches under one gate. It lands
each plan as `land` does, then cuts `--branch` (default `fleet/integration`) and
re-runs every merged unit's accept against the combined tree. `--accept-timeout`
bounds every gate it runs, the per-plan one and the combined one.

It refuses with exit 2 when the plans do not share one repo root and one base,
because land merges in one checkout. It also refuses `--branch` when it is a
base branch, a plan's own `<encounter>/integration`, a lane branch, a branch
that already exists, or a worktree holding another branch: each of those would
move someone's work. An encounter named `fleet` therefore needs `--branch`. A
conflict names the plan and stops, and the fleet branch is never reported as
good.

`up --all` spawns every plan's minions, one workspace per encounter, then runs
one `next --all` round unless `--no-dispatch` is passed or a spawn failed.

A settled encounter, whose units are all `done` or `canceled`, is left alone:
`up --all` spawns no lane for it, writes no state and prints `<encounter>:
settled, no lanes spawned`. That is not a failure. Run it over a repo with
finished plans and nothing wakes.

An interrupted sweep prints `interrupted: lanes still working, run dungeon next
--all again` and exits 2. Nothing is claimed: the unit stays `working`, because
a gate cut short was never judged. Run the sweep again.

The stop is prompt. An interrupt during a `--poll` wait returns at once instead
of waiting the interval out, so a long poll never delays your stop.

## Supervise at short intervals

A lane in a doomloop burns context and tokens fast. A five minute wait is five
minutes of that. Never block longer than 60 to 90 seconds on a watch.

- Pass the timeout explicitly. `dungeon_watch` defaults to 60 seconds, and a
  bare `dm msg wait` defaults to whatever you typed. Set `timeout: 60` on every
  call. Do not accept the default.
- A timeout that names the same cursor every time is not a quiet fleet. Check
  the box's newest seq with `dm msg list --limit 1 --json`. A cursor ahead of it
  means the box was reset, or it came from another session; call `dungeon_watch`
  with `reset: true`.
- A timeout is not a failure. It is a cue to look. Run `dungeon status`, then
  read the pane of every `working` lane.
- Read the pane, not just the state. A lane can be `working` and looping. The
  state only says a lane is alive.
- Look for the loop: the same command twice, the same file read again, an error
  that does not change, a retry of an accept that already failed, no new tool
  call between two looks.
- Kill and re-dispatch on the second look at the same loop. `dungeon down`, then
  re-queue the unit or write a new one. Do not wait for the lane to recover.

Between looks, do plan work: read reports, split units, answer `blocked` lanes.
Do not idle on a long wait.

## Agent names

The minion id is your handle. Briefs, reports, worktrees, sessions, state keys
and every instruction you write use it. The multiplexer sees the derived agent
name instead: `<minion>-<token>`.

The token is exactly two characters from `[0-9a-z]`. `dungeon up` generates it
on the first spawn and stores it as `token` in the run's `state.json`. The agent
name is the minion id truncated to 29 characters, a hyphen, then the token. The
32 character limit is dungeon's own rule, not a backend's, so every backend
accepts the name. The minion id alphabet is already lowercase.

Agent names are global within one multiplexer. `up` checks every derived name
against the live fleet and re-rolls the whole token when one is taken, up to 64
draws, then fails with a finding that names the live agent. Two encounters may
declare the same minion ids; the token is what keeps them on separate agents.

Consequences for you:

- Never hardcode an agent name in a plan, a brief or a command. Write the
  minion id; dungeon derives the rest.
- To reach a lane, use `space f`, `dungeon status`, or the pane recorded in the
  state. Do not address a multiplexer with a name you guessed.
- A run already in flight without a token keeps the bare minion id. `up` leaves
  its live bare agents alone instead of renaming the fleet under it, so do not
  expect a token on an old run.

Two minion ids that collide after truncation fail `dungeon check` with
`E_DUPLICATE_AGENT`. Shorten one id.

## Lanes notify you

Every brief tells the lane to write its report and then post to the session box:

```sh
dm msg post --key lane/<encounter>/<minion> --state done --msg "<unit>: <minion> finished. Report: <path>"
```

The key carries the encounter, because two encounters in one fleet may both
declare `w1`. Watch the `lane/` prefix and you see both.

The parent reads the box. It does not type into your pane.

A box entry is a wake-up, not a verdict. Run `dungeon next` to settle the unit:
the report and the unit's accept command or verdict still decide. Then read the
report.

In pi, call the `dungeon_watch` tool instead, with `timeout: 60`. It drains
entries past the persisted cursor in `.dungeon/box.cursor.<session>`, then
blocks for the next one. The cursor is a bare seq, so it is only valid against
the box that wrote it: the tool compares it with the newest entry and resets a
cursor that sits ahead of the box, which is what a reset box or an earlier
session leaves behind. The first call starts at the box's newest entry, so a
new encounter does not replay old events. After a `done` or `error` event, run
`dungeon next`. After a `blocked` event, read the lane's pane and answer it.

The extension lives in this repo at `skills/dungeon/dungeon-watch.ts`. Symlink
it into `~/.pi/agent/extensions/` and reload pi to load it.

Anywhere else, read the box with a cursor:

```sh
dm msg list --prefix lane/ --after <cursor> --json
```

Persist the cursor. A cursor is a bare seq, so a box that was reset leaves it
ahead of every entry and `dm msg wait --after` never fires. Do not use
`dm msg wait --prefix lane/ --state blocked` for a fleet: without `--after` it
reads the newest entry under the prefix, so a newer `done` on another lane
masks an older `blocked`. Use `dm msg sub --prefix lane/ --json` for a stream,
or wait per key.

The box is durable. A lane that posts `done` and exits is still visible. The
report file remains the record.

Every brief lists the reports from the unit's deps under `reports from deps`,
one absolute path per dep. Point the lane at those files instead of asking it
to search. A fix unit's report path is the review report it must read.

`space f` jumps to the selected unit's lane. Use it on a `blocked` row: the
lane is asking the operator something, and the answer needs their eyes on that
pane.

The view is modal: `space` opens a which-key pane bottom right. `esc` closes
whatever is open and never quits the app; `ctrl+c` quits.

You may pin a minion to a pane the operator is already watching:
`- {id: w1, role: worker, worktree: w1, pane: "w5N:pH"}`. A pin is a request.
`up` checks that the pane still exists and splits a fresh one if it does not,
so a stale plan never blocks a run.

## Read the state before you act

| state | meaning | your move |
| --- | --- | --- |
| `ready` | deps landed, no lane on it | nothing. `next` dispatches it |
| `waiting` | deps not landed | nothing. `next` handles it. A cross-plan dep on a `failed` unit never releases: fix it there |
| `working` | a lane is on it, or `next` is running its accept | nothing. Do not help |
| `blocked` | needs a human, or the plan is wrong | read the note, then act |
| `done` | proved: accept exited 0, or the verdict line was found | nothing |
| `unproven` | settled, with no accept or verdict; a provisional fix lands here on purpose | nothing for a provisional fix. Otherwise add an `accept` or a `verdict` |
| `rejected` | the accept command failed | send it back, see below |
| `failed` | a review report lacked its pass line | nothing in a loop. The next fix releases |
| `canceled` | withdrawn | nothing |

`done`, `failed` and a provisional `unproven` release dependents. Nothing
upgrades a claim to a pass.

## When a unit is rejected or blocked

The lane owns the fix. Your moves, in order:

1. Read the note, then the report: `dungeon view`, then `d` on the unit.
2. If the note says the work is uncommitted or outside the unit's paths, the
   brief or the plan is wrong. Fix the plan with `dungeon edit`, never by hand
   and never the worktree: `dungeon edit unit <id> --paths-add '<glob>'` widens
   what it owns, `--accept '<cmd>'` gives it a gate. `next` prints a `plan fix:`
   line when a blocked or unproven unit is waiting on a plan edit: out-of-paths
   files want wider `paths`, a missing accept wants an `accept` command. Lead
   your report to the operator with that line. It is a plan change, not a
   stalled watch.
3. Re-queue: `dungeon set unit <id> planned --note "retry: <why>"`, then
   `dungeon next --force`: the plan edit moved the plan hash, so `next` refuses
   it without the flag.
4. If the unit itself is wrong, split it, reassign it, or cancel it. That is
   plan work, and plan work is yours.
5. If it needs a decision only the operator can make, stop and ask. Do not
   guess, and do not implement it yourself.

## Never

- Implement, fix or "just quickly" adjust a unit yourself. Not even a one-line
  fix. Re-queue it, or write a new unit for it.
- Dispatch into a lane that does not exist. `no lane` means run `dungeon up`.
- Edit the plan mid-run to give yourself work. If the plan is wrong, say so and
  fix the plan with the operator watching.
- Merge by hand. `dungeon land` merges in declaration order and re-runs every
  accept against the merged result.
- Merge a fleet by hand either. `dungeon land --all` lands every encounter and
  merges their integration branches under one combined gate. Two `git merge`
  calls in a parent are a fleet land that nothing re-proved.
- Fan one blocking `up` or `next` out per encounter from a parent's tool layer.
  That is the workaround `up --all` and `next --all` replace: the parent goes
  blind for the whole sweep and cannot answer a blocked lane until the last
  call returns.
- Push. `land` leaves a local branch and says so. The operator decides what
  reaches the default branch.
- Claim a unit passed. Quote the accept command and its exit code.

## Files worth knowing

```
.dungeon/plan.yaml     the plan. belongs in git
.dungeon/state.json    the run. ignored
.dungeon/briefs/       what each lane was told
.dungeon/reports/      what each lane said
.dungeon/sessions/     the lane's pi sessions
```

## Reading a unit in the view

`d` opens the detail pane for the selected unit: goal, deps, note, history, the
files kept with it, and its link. `j`/`k` or `tab` picks a file, `enter` reads it, `esc`
steps back, `space o` opens the unit's link in the browser, `space f` jumps to
its lane.

On a loop summary row, `d` or `enter` opens the round timeline instead of a unit
card. From there `enter` opens a round's unit card, `esc` goes back to the
timeline, and `space f` jumps to the selected round's lane.

A brief or a report is markdown, and the reader renders it once at open, and
again only when the pane width changes, never per frame. Headings, links,
inline code and code blocks keep their colour; prose stays plain because the
sparse style avoids colouring every prose run. Write them as markdown and the
operator sees structure instead of a wall of text. A `.log` or any other
extension is shown as plain text, because a build log is not prose.

Only files are rendered. A unit's goal and note are plain text in the card, so
do not put markdown in them expecting it to be styled.

Attach anything else a unit needs, so the operator can read it from the view:
`dungeon attach <unit> <path>`.

When a unit has a pull request, link it so the operator can reach it in one
keystroke: `dungeon link <unit> <url>`. The row is marked `↗` and `space o` opens it.
The URL is a plain http(s) link, so any forge works. Do not paste it into chat
instead.

## Opening the view

`dungeon view` is a TUI, and it finds the plan by walking up, so it opens from
anywhere in the repo. Run it in its own pane beside the lanes:

```bash
dm split <pane> --dir right
dm write-to <new-pane> --msg "dungeon view" --enter
```

Rows are a two column table sized to their content, with a two column gap, so a
wide terminal shows a block rather than a stretched line. `space f` then jumps
from the graph to a lane. Tell the operator to look there when a unit is
`blocked`; do not paste pane output into chat.

## The fleet view

`dungeon fleet` shows every encounter under `.dungeon` in one window, newest
touch first. It takes no plan and needs no argument.

Open it instead of `dungeon view` in two cases:

- More than one encounter is live or parked. One window shows them all.

- The lanes run in a workspace you are not watching. The fleet preview reads
  the pane itself, so it still says what the lane is doing.

Keys:

| key | action |
| --- | --- |
| `j`/`k`, down/up | move |
| `g`/`home`, `G`/`end` | first row, last row |
| `enter`, `d` | detail for the selected row |
| `space` | actions |
| `q`, `ctrl+c` | quit |
| `esc` | close the open overlay, never quit |

The actions pane has four moves: `j` jumps to the lane, `h` opens the hunk
session on its worktree, `c` copies the pane id, and `e` expands or folds the
settled units.

Rows sit under BLOCKED, WORKING, READY and SETTLED. A broken plan is named
under BLOCKED with its error. Settled rows fold to one summary row per
encounter until you expand them.

The preview under the list has two sources. It reads the selected lane's pane
once per refresh tick. When the pane is gone or unreadable it shows the lane's
report, `.dungeon/reports/<unit>.md`, and the header names the source it read.

The fleet view never writes. It reads plans, state, reports and panes. A jump
moves the multiplexer's focus and nothing else: no key on this screen
dispatches, settles or edits a lane.

## The rail

`dungeon rail` is a tmux-only surface. It is not part of the dm path. Use
`dungeon view` or `dungeon fleet` in a dm pane instead.

## Finishing

```bash
dungeon land          # merge done units, re-run every accept against the result
dungeon down          # close the workspace, keep the run
dungeon clear         # delete the finished plans, keep the runs
```

`land` records `landed {ref, commit, at}` for every merged unit whose gate
passed. A plan in another repo waits on that record, so a cross-plan dep
releases only after land runs here. Marking a unit done is not enough.

Report to the operator: what landed, what is blocked and why, and which branch
holds the result. Name the units, not the files.

`dungeon clear` retires an encounter once every unit is done or canceled and no
lane is live. It deletes the plan and nothing else, because a brief and a report
are the record of what was done. Add `--remove` to delete the state, the briefs,
the reports and the sessions too, and to sweep the run of a plan cleared earlier.
Add `--remove-worktrees` to delete the lane worktrees and their
`<encounter>/<minion>` branches; it is never implied by `--remove`, because a
lane branch can hold work that never landed. It sweeps every encounter under
`.dungeon` unless `--plan` names one, and `--dry-run` says what would go without
touching anything. A plan with a live lane is refused: run `dungeon down` first.

## A worked split

A task that reads like "fix the auth timeouts across the API and the web app":

1. `recon` on a scout: where are the timeouts set? One lane, no writes.
2. `api-timeout` on a worker, `deps: [recon]`, `paths: [apps/api/**]`.
3. `web-timeout` on a second worker, `deps: [recon]`, `paths: [apps/web/**]`.
4. `review-api` and `review-web` on two reviewers, each paired with its worker.
5. `dungeon land` after both verdicts: it merges the workers' branches and re-runs the repo gate.

Two writing lanes, two verdicts, one merge gate. You dispatch it and read it. The
parent's only edit in that whole run is the plan.

## One-off delegation

Use a pane for one second opinion, one fresh-context review, or one recon. If
the work splits into units, it is an encounter, not a pane.

Write the handoff to `${TMPDIR:-/tmp}/dungeon-handoffs/<slug>/handoff.md` before
any pane exists, outside the checkout. It states the objective, repo and cwd,
what the child may change, the acceptance criteria, and the stop conditions. The
child prompt only points at that file.

Split one pane in your own cwd and take the pane id dm prints. Never derive it
from an example:

```bash
pane=$(dm split "$DM_PANE" --dir right)
```

Start the child in that pane with an explicit tool allowlist. A read-only child
gets `read,bash`, no `edit` and no writer, so its findings go in its final reply.
A writer child may keep a report under
`${TMPDIR:-/tmp}/dungeon-handoffs/<slug>/`, outside the checkout:

```bash
dm write-to "$pane" --msg 'pi --no-skills --no-mcp --tools read,bash' --enter
```

Prompt it to read the handoff and do it, then wait on the box:

```bash
dm write-to "$pane" --msg "Read <abs>/handoff.md and do it." --enter
dm msg wait --key handoff/<slug> --state done --timeout 15m
```

The child posts when it finishes:

```bash
dm msg post --key handoff/<slug> --state done --msg "<report path>"
```

A wait that times out means the task is async, not that it failed. Check it again
later.

Read the child's reply from the pane. A writer child's report file comes first;
fall back to the pane only when there is no report:

```bash
dm read "$pane" --lines 120
```

Close only the pane you opened: `dm exec '{"action":"close-pane","target":"<pane>"}'`.

Never commit, push or merge from a child, and keep one writer per checkout.

When the child is `blocked`, it is asking a question. Answer with raw pane input,
not another prompt, then wait again:

```bash
dm write-to "$pane" --msg "<answer>" --enter
```
