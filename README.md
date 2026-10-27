# dungeon

A plan that dispatches. One file declares the encounter, the minions, and the
units of work; the tool spawns the minions, pings each one when its turn comes,
runs the acceptance command, and never writes code itself.

**The DM does not play a minion.** A unit that touches files must name a minion
that owns a worktree. Anything else fails validation.

## Status

Every command is built: `init`, `check`, `status`, `set`, `view`, `attach`, `up`,
`next`, `land` and `down`.

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

dungeon attach MOL1 /tmp/brief-mol1.md

dungeon up                         # spawn the lanes, then one round of next
dungeon up --no-dispatch           # spawn only
dungeon next --dry-run             # say what would go out
dungeon next --timeout 10m

dungeon land                       # merge done units, re-run their accepts
dungeon down                       # close the lanes, keep the run
```

A dispatch is five steps and only the last one can mark a unit `done`:

1. write `.dungeon/briefs/<unit>.md`: goal, owned paths, worktree, accept
   command, and what the lane may not touch;
2. submit it with `herdr agent prompt`;
3. wait for the lane to settle;
4. read `.dungeon/reports/<unit>.md`. No report means `blocked`, never `done`;
5. run the unit's `accept` command in the lane's worktree. Exit 0 is `done`,
   anything else is `rejected`, and no command at all is `unproven`.

`up` is idempotent: a minion whose agent is live is left alone, so a second `up`
resumes instead of doubling the fleet. A writer gets its own git worktree at
`<repo>/../<encounter>-<minion>` on branch `<encounter>/<minion>`. Lanes run with
`--no-skills --no-mcp` and a tool allowlist that matches their role: `read,bash`
for staff work, `read,bash,edit,write` for writers.

`land` merges the `done` units into `<encounter>/integration` in declaration
order, aborts on the first conflict, then re-runs each merged unit's accept
against the merged result. It never touches the base branch and never pushes.
`down` closes the lanes and the encounter tab, refuses to stop a working lane
unless you pass `--force`, and leaves the plan, state, briefs and reports.

`d` opens the detail pane for the selected unit: goal, deps, note, history, and
the files kept with it. Two are found by convention,
`.dungeon/briefs/<unit>.md` and `.dungeon/reports/<unit>.md`; anything else comes
from `dungeon attach`. `tab` picks a file, `enter` reads it in place with `j`/`k`
scrolling, `esc` goes back. A missing brief is shown as missing, not hidden.

`view` is the same snapshot as `status`, rendered with charmbracelet. Rows carry
a right-aligned status column (`working w5M:p8`, `waits TOK4`, `needs you`), and
a boxed strip under the list holds everything about the selected unit: goal,
deps, note, and where its lane is. Two layouts, `t` and `s`:

- **tree** (default): dependencies as nesting, drawn with box-drawing
  connectors. Each unit hangs under its deepest dependency, ties going to the
  later one, so every unit appears exactly once and a connector means "this one
  unblocks that one". A fan-in shows `∧N` on
  the joining unit, and the panel lists every input with its status.
- **sections**: urgency-first groups, BLOCKED down to CANCELED.

`space` folds the selected branch. A foldable row is marked `▾`, a folded one
`▸` with `[+N]` for how much it hides, and the header counts the folds in force,
so a collapsed branch is never silent. `c` collapses every branch, `e` expands
everything. Folds survive a refresh; they
are a view of the session, not part of the state file. `h` hides done and
canceled units and splices their children into the parent, so a settled chain
collapses without leaving orphaned connectors.

The cursor follows a unit across a refresh, not a row. `?` opens the legend: what
each glyph means, and every key. `j`/`k` move, `g`/`G` jump, `r` refreshes, `q`
quits. Piped output prints one frame instead of opening
a TUI.

`dungeon attach <unit> <path>` keeps a file one keystroke from the unit: a
brief, a report, a screenshot, a handoff note. It stores an absolute path, so
the file stays readable wherever the plan is opened from.

Unit statuses: `planned`, `working`, `finished`, `done`, `unproven`,
`rejected`, `blocked`, `canceled`. **`done` means one thing: the unit's `accept`
command exited 0 in its minion's worktree.** No command, no done: the unit goes
`unproven` and its dependents stay blocked.

One encounter is one directory: `.dungeon/` holds `plan.yaml`, `state.json`,
`briefs/` and `reports/`. Nothing goes to `$XDG_STATE_HOME`, so a brief survives
until the encounter is archived and `dungeon down` cannot lose it. The pane column comes from
`herdr agent list` at read time, matched by agent name, so it cannot go stale.

Exit codes: `0` clean, `1` findings, `2` could not run (missing file, bad usage).

## Plan

See `testdata/plan.yaml`. Roles: `scout` and `reviewer` produce facts and
verdicts; `worker` and `integrator` own a worktree and write.

## Tested against a real dagr run

`testdata/dagr-ll-frontend-raw.json` is `ll-frontend/.dagr/run.json` (dagr v3,
32 tasks, 6 projects) converted mechanically. It produces 22 findings, and
those findings are the point:

- 14 of 32 tasks carry no owner at all, so they name no minion.
- 8 tasks have an owner but no ownership paths, so nothing says what they own.

dagr cannot express either fact, which is why an orchestrator is free to pick
the work up.

`testdata/dagr-ll-frontend-mapped.yaml` is the same run set up correctly: 30
units, 16 minions, every unit assigned, every worker unit owning paths, and the
dependency graph unchanged. It validates clean. Gates become reviewer verdicts;
the single `orchestrator` owner becomes one gatekeeper plus one integrator. A
test compares the two plans unit by unit, so the graph cannot drift.

## The skill

`skills/dungeon/SKILL.md` teaches a pi agent to run this tool, and its first
rule is the one that matters: the parent orchestrates and does not do the work.
Link it where pi looks for skills:

```sh
ln -s ~/Code/Personal/dungeon/skills/dungeon ~/.agents/skills/dungeon
```

The skill covers writing a plan, the dispatch loop, what each state means,
what to do when a unit is rejected, and the moves a parent must never make.

## Design

`~/.agents/store/plans/changes/pi/dungeon/README.md`.
