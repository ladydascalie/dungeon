---
name: dungeon
description: Default mode for any task that needs decomposition: if it cannot be fixed inline in one pass, it becomes an encounter. A tracked plan of units, one minion per unit, a graph you watch. Orchestrate and delegate with dungeon, the plan-and-dispatch tool. The parent lane writes the plan, spawns minions, reads the graph and lands the result, and never does the work itself. Use when asked to plan, dispatch, monitor or land work with dungeon, when a .dungeon/plan.yaml exists, or when a task should be split across parallel lanes with a mechanical definition of done.
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
no decomposition, so it stays one herdr pane (the `herdr-delegation` skill).
Everything else becomes an encounter, however small the split.

Coalition is not a mode we run. It failed in practice: too many moving parts
for the control it gave. Do not propose it.

## The one rule

A unit that touches files names a minion that writes. You may write exactly two
things: the plan, and your notes. You may not edit a unit's paths, a worktree, or
a branch.

## Gate before you start

```bash
command -v dungeon          # required
test "${HERDR_ENV:-}" = 1   # required: dungeon drives herdr panes
```

Outside herdr, say so and stop. Dungeon cannot spawn a lane without it.

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
concurrency: 3

minions:
  - {id: scout, role: scout, tools: "read,bash,grep,find,ls"}  # facts. no writes
  - {id: w1, role: worker, worktree: w1}            # writes. own worktree
  - {id: r1, role: reviewer, of: w1}                # reads w1. gives a verdict
  - {id: land, role: integrator, worktree: int}     # merges. own worktree

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
  - id: land
    minion: land
    deps: [review-api]
    accept: mise run gate
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
- **A real acceptance command.** It runs in the lane's worktree. Exit 0 makes
  the unit done. A reviewer may give a `verdict` line instead, and that exact
  report line makes its unit done. No accept and no verdict means `unproven`,
  which blocks every dependent.
- **A small unit.** A unit a lane can finish in one sitting gets a report worth
  reading.

More than one encounter under `.dungeon`: `dungeon view` opens a picker, and
every other command needs `--plan`. Extra encounters live at
`.dungeon/<name>/plan.yaml` with their own state, briefs and reports. A lane
passes the full path; there is no picker outside the view.

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
`auth-fix-2` and `auth-review-3`. The first review has no deps. Each fix
depends on its review. Each review after the first depends on the fix before it.

`rounds` is 1 to 9. `focus`, `pass` and `paths` are required. `reviewer` must be
a reviewer and `fixer` must be a writer. `paths` are the files the fixer owns.
`review_goal` and `fix_goal` are optional. Without them the generated goals name
the round, the focus and the report to read.

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

## The loop

```bash
dungeon up            # spawn a lane per minion, then one round of next
dungeon next          # settle what landed, dispatch what is ready, wait
dungeon status        # who is working, who is not, which pane
dungeon view          # live graph. d detail, space actions. picker when >1
```

Run `dungeon next` until it says nothing to do. It is idempotent and safe in a
loop, and there is no daemon to keep alive.

`up` builds one workspace per encounter, one tab per role and one pane per
minion, so the herdr layout reads like the plan. Panes grow to the right unless
the plan says `split: down`, and they are equal shares of the tab.

`dungeon up` is idempotent too. A minion whose agent is live is left alone, so a
second `up` resumes the fleet instead of doubling it.

## Lanes ping you

Every brief tells the lane to write its report and then run `herdr pane run
<your pane> "<unit> finished. Report: <path>"`. dungeon writes your pane id
from `$HERDR_PANE_ID` at dispatch, so run dungeon from the pane that should
receive the ping.

A ping is a wake-up, not a verdict. Run `dungeon next` to settle the unit: the
report and the unit's accept command or verdict still decide. Then read the
report.

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
| `waiting` | deps not landed | nothing. `next` handles it |
| `working` | a lane is on it | nothing. Do not help |
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
   brief or the plan is wrong. Fix the plan. Never edit the worktree.
3. Re-queue: `dungeon set unit <id> planned --note "retry: <why>"`, then
   `dungeon next`.
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

A brief or a report is markdown, and the reader renders it: headings, lists,
emphasis and code blocks all survive. Write them as markdown and the operator
sees structure instead of a wall of text. A `.log` or any other extension is
shown as plain text, because a build log is not prose.

Only files are rendered. A unit's goal and note are plain text in the card, so
do not put markdown in them expecting it to be styled.

Attach anything else a unit needs, so the operator can read it from the view:
`dungeon attach <unit> <path>`.

When a unit has a pull request, link it so the operator can reach it in one
keystroke: `dungeon link <unit> <url>`. The row is marked `↗` and `space o` opens it.
The URL is a plain http(s) link, so any forge works. Do not paste it into chat
instead.

## Opening the view in herdr

`prefix+d` opens `dungeon view` in a right split, from anywhere in the repo:
the plan is found by walking up. Rows are a two column table sized to their content, with a
two column gap, so a wide terminal shows a block rather than a stretched line.
`space f` then jumps from the graph to a lane.
Tell the operator to look there when a unit is `blocked`; do not paste pane
output into chat.

## Finishing

```bash
dungeon land          # merge done units, re-run every accept against the result
dungeon down          # close the workspace, keep the run
```

Report to the operator: what landed, what is blocked and why, and which branch
holds the result. Name the units, not the files.

## A worked split

A task that reads like "fix the auth timeouts across the API and the web app":

1. `recon` on a scout: where are the timeouts set? One lane, no writes.
2. `api-timeout` on a worker, `deps: [recon]`, `paths: [apps/api/**]`.
3. `web-timeout` on a second worker, `deps: [recon]`, `paths: [apps/web/**]`.
4. `review-api` and `review-web` on two reviewers, each paired with its worker.
5. `land` on the integrator, `deps: [review-api, review-web]`, with the repo gate.

Four writing lanes, two verdicts, one merge. You dispatch it and read it. The
parent's only edit in that whole run is the plan.
