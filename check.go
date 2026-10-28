package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Codes are stable: a plan's problems are addressed by code, not by prose.
type Code string

const (
	EParse         Code = "E_PARSE"
	ENoEncounter   Code = "E_NO_ENCOUNTER"
	EBadEncounter  Code = "E_BAD_ENCOUNTER"
	ENoMinion      Code = "E_NO_MINION"
	EUnknownMinion Code = "E_UNKNOWN_MINION"
	EUnknownRole   Code = "E_UNKNOWN_ROLE"
	EDuplicateID   Code = "E_DUPLICATE_ID"
	EDuplicatePane Code = "E_DUPLICATE_PANE"
	EBadMinionID   Code = "E_BAD_MINION_ID"
	ENoID          Code = "E_NO_ID"
	ENoWorktree    Code = "E_NO_WORKTREE"
	EDMPlaysMinion Code = "E_DM_PLAYS_MINION"
	ENoPaths       Code = "E_NO_PATHS"
	EMissingDep    Code = "E_MISSING_DEP"
	ECycle         Code = "E_CYCLE"
	ESplitInvalid  Code = "E_SPLIT_INVALID"
	ESplitNoMinion Code = "E_SPLIT_NO_MINION"
	EUnknownSplit  Code = "E_UNKNOWN_SPLIT"
	EBadTools      Code = "E_BAD_TOOLS"
	EVerdictRole   Code = "E_VERDICT_ROLE"
	EVerdictAccept Code = "E_VERDICT_ACCEPT"
	ELoopRole      Code = "E_LOOP_ROLE"
	ELoopRounds    Code = "E_LOOP_ROUNDS"
	ELoopPass      Code = "E_LOOP_PASS"
	ELoopFocus     Code = "E_LOOP_FOCUS"
	EOfNotWriter   Code = "E_OF_NOT_WRITER"
)

type Finding struct {
	Code    Code   `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Path == "" {
		return fmt.Sprintf("%s %s", f.Code, f.Message)
	}
	return fmt.Sprintf("%s %s: %s", f.Path, f.Code, f.Message)
}

// A minion id becomes a herdr agent name, so it obeys that alphabet.
var minionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// A tool name is what pi accepts after --tools.
var toolName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// The directions herdr can split a pane in.
var splits = map[string]bool{"right": true, "down": true, "left": true, "up": true}

// An encounter becomes a branch prefix, a tab label and a directory name, so it
// is restricted to what all three accept.
var encounterID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Validate returns every violation in the plan, in declaration order.
func Validate(p Plan) []Finding {
	var f []Finding

	switch {
	case p.Encounter == "":
		f = append(f, Finding{ENoEncounter, "encounter", "name the encounter"})
	case !encounterID.MatchString(p.Encounter):
		f = append(f, Finding{
			EBadEncounter, "encounter",
			fmt.Sprintf("encounter %q must match [a-z0-9][a-z0-9-]{0,40}: it becomes a branch prefix", p.Encounter),
		})
	}
	if p.Split != "" && !splits[p.Split] {
		f = append(f, Finding{
			EUnknownSplit, "split",
			fmt.Sprintf("split %q is not one of right, down, left, up", p.Split),
		})
	}
	if len(p.Minions) == 0 {
		f = append(f, Finding{ENoMinion, "minions", "a plan with no minions dispatches nothing"})
	}

	roleOf := map[string]string{}
	minionIDs := map[string]bool{}
	for i, m := range p.Minions {
		at := fmt.Sprintf("minions[%d]", i)
		switch {
		case m.ID == "":
			f = append(f, Finding{ENoID, at + ".id", "minion needs an id"})
		case !minionID.MatchString(m.ID):
			f = append(f, Finding{
				EBadMinionID, at + ".id",
				fmt.Sprintf("minion id %q must match [a-z][a-z0-9_-]{0,31}", m.ID),
			})
		}
		if minionIDs[m.ID] {
			f = append(f, Finding{EDuplicateID, at + ".id", fmt.Sprintf("minion id %q is already used", m.ID)})
		}
		minionIDs[m.ID] = true

		if !roles[m.Role] {
			f = append(f, Finding{
				EUnknownRole, at + ".role",
				fmt.Sprintf("role %q is not one of scout, worker, reviewer, integrator", m.Role),
			})
		}
		if writes(m.Role) && m.Worktree == "" {
			f = append(f, Finding{
				ENoWorktree, at + ".worktree",
				fmt.Sprintf("role %q writes, so minion %q needs its own worktree", m.Role, m.ID),
			})
		}
		roleOf[m.ID] = m.Role

		if m.Tools != "" {
			f = append(f, checkTools(at, m.Tools)...)
		}
	}

	for i, m := range p.Minions {
		if m.Of == "" {
			continue
		}
		at := fmt.Sprintf("minions[%d].of", i)
		if !minionIDs[m.Of] {
			f = append(f, Finding{EUnknownMinion, at, fmt.Sprintf("minion %q is not declared", m.Of)})
		}
		if m.Of == m.ID {
			f = append(f, Finding{ECycle, at, "a minion cannot be its own pair"})
		} else if role, ok := roleOf[m.Of]; ok && roles[role] && !writes(role) {
			f = append(f, Finding{
				EOfNotWriter, at,
				fmt.Sprintf("minion %q has role %q, which does not write, so it has no worktree to read", m.Of, role),
			})
		}
	}

	unitIDs := map[string]bool{}
	rep := map[string]Unit{} // first declaration of each id, for dep checks
	for i, u := range p.Units {
		at := fmt.Sprintf("units[%d]", i)
		switch {
		case u.ID == "":
			f = append(f, Finding{ENoID, at + ".id", "unit needs an id"})
		case unitIDs[u.ID]:
			f = append(f, Finding{EDuplicateID, at + ".id", fmt.Sprintf("unit id %q is already used", u.ID)})
		}
		unitIDs[u.ID] = true
		if _, dup := rep[u.ID]; !dup {
			rep[u.ID] = u
		}
	}

	for i, u := range p.Units {
		at := fmt.Sprintf("units[%d]", i)
		role := ""
		if u.Split != nil {
			if u.Minion != "" {
				f = append(f, Finding{
					ESplitInvalid, at + ".minion",
					"a split unit binds its children at dispatch time, so it names no minion",
				})
			}
			if u.Split.Role != RoleWorker {
				f = append(f, Finding{
					ESplitInvalid, at + ".split.role",
					fmt.Sprintf("split role %q must be worker: a split creates worker units", u.Split.Role),
				})
			}
			if u.Split.Count < 1 {
				f = append(f, Finding{ESplitInvalid, at + ".split.count", "split needs a count of at least 1"})
			}
			if u.Split.Role == RoleWorker {
				if n := countRole(p.Minions, u.Split.Role); n < u.Split.Count {
					f = append(f, Finding{
						ESplitNoMinion, at + ".split.count",
						fmt.Sprintf("split needs %d %s minions, the plan declares %d",
							u.Split.Count, u.Split.Role, n),
					})
				}
			}
		} else {
			switch {
			case u.Minion == "":
				f = append(f, Finding{ENoMinion, at + ".minion", "unit names no minion"})
			default:
				r, ok := roleOf[u.Minion]
				if !ok {
					f = append(f, Finding{
						EUnknownMinion, at + ".minion",
						fmt.Sprintf("minion %q is not declared", u.Minion),
					})
				}
				role = r
			}
			if len(u.Paths) > 0 && role != "" && !writes(role) {
				f = append(f, Finding{
					EDMPlaysMinion, at + ".paths",
					fmt.Sprintf("the DM does not play a minion: role %q may not declare paths", role),
				})
			}
			if needsPaths(role) && len(u.Paths) == 0 {
				f = append(f, Finding{
					ENoPaths, at + ".paths",
					fmt.Sprintf("worker unit %q must declare the paths it owns", u.ID),
				})
			}
		}

		if u.Verdict != "" {
			if role != RoleReviewer {
				f = append(f, Finding{
					EVerdictRole, at + ".verdict",
					fmt.Sprintf("unit %q sets a verdict but its role is %q: only a reviewer proves a unit", u.ID, role),
				})
			}
			if u.Accept != "" {
				f = append(f, Finding{
					EVerdictAccept, at + ".accept",
					"a verdict unit is proven by the reviewer's report line, so it cannot also set accept",
				})
			}
		}

		for j, d := range u.Deps {
			where := fmt.Sprintf("%s.deps[%d]", at, j)
			if d == u.ID {
				f = append(f, Finding{ECycle, where, "unit depends on itself"})
				continue
			}
			if !unitIDs[d] {
				f = append(f, Finding{
					EMissingDep, where,
					fmt.Sprintf("dep %q is not a declared unit", d),
				})
			}
		}
	}

	f = append(f, checkLoops(p.Loops, roleOf, unitIDs)...)
	f = append(f, duplicatePanes(p.Minions)...)
	return append(f, cycles(p.Units, rep)...)
}

// checkTools validates a minion's tool allowlist: names, no empties, no
// duplicates. An empty string means the role default and is not checked here.
func checkTools(at, tools string) []Finding {
	var f []Finding
	seen := map[string]bool{}
	for i, raw := range strings.Split(tools, ",") {
		name := strings.TrimSpace(raw)
		switch {
		case name == "":
			f = append(f, Finding{
				EBadTools, at + ".tools",
				fmt.Sprintf("tool %d is empty: an empty item names no tool", i+1),
			})
			continue
		case !toolName.MatchString(name):
			f = append(f, Finding{
				EBadTools, at + ".tools",
				fmt.Sprintf("tool %q must match [a-z_][a-z0-9_]*", name),
			})
		}
		if seen[name] {
			f = append(f, Finding{
				EBadTools, at + ".tools",
				fmt.Sprintf("tool %q is listed twice", name),
			})
		}
		seen[name] = true
	}
	return f
}

// checkLoops validates each loop without expanding it: another unit owns the
// expansion, so this only proves the loop could be expanded.
func checkLoops(loops []Loop, roleOf map[string]string, unitIDs map[string]bool) []Finding {
	var f []Finding
	loopIDs := map[string]bool{}
	for i, l := range loops {
		at := fmt.Sprintf("loops[%d]", i)
		switch {
		case l.ID == "":
			f = append(f, Finding{ENoID, at + ".id", "loop needs an id"})
		case unitIDs[l.ID]:
			f = append(f, Finding{
				EDuplicateID, at + ".id",
				fmt.Sprintf("loop id %q is already used by a unit", l.ID),
			})
		case loopIDs[l.ID]:
			f = append(f, Finding{
				EDuplicateID, at + ".id",
				fmt.Sprintf("loop id %q is already used by another loop", l.ID),
			})
		}
		loopIDs[l.ID] = true

		role, ok := roleOf[l.Reviewer]
		switch {
		case l.Reviewer == "":
			f = append(f, Finding{ENoMinion, at + ".reviewer", "loop names no reviewer"})
		case !ok:
			f = append(f, Finding{
				EUnknownMinion, at + ".reviewer",
				fmt.Sprintf("minion %q is not declared", l.Reviewer),
			})
		case role != RoleReviewer:
			f = append(f, Finding{
				ELoopRole, at + ".reviewer",
				fmt.Sprintf("loop reviewer %q has role %q, so it cannot review", l.Reviewer, role),
			})
		}

		fixRole, ok := roleOf[l.Fixer]
		switch {
		case l.Fixer == "":
			f = append(f, Finding{ENoMinion, at + ".fixer", "loop names no fixer"})
		case !ok:
			f = append(f, Finding{
				EUnknownMinion, at + ".fixer",
				fmt.Sprintf("minion %q is not declared", l.Fixer),
			})
		case !writes(fixRole):
			f = append(f, Finding{
				ELoopRole, at + ".fixer",
				fmt.Sprintf("loop fixer %q has role %q, so it cannot write a fix", l.Fixer, fixRole),
			})
		}

		if l.Rounds < 1 || l.Rounds > 9 {
			f = append(f, Finding{
				ELoopRounds, at + ".rounds",
				fmt.Sprintf("loop rounds is %d: give 1 to 9", l.Rounds),
			})
		}
		if strings.TrimSpace(l.Pass) == "" {
			f = append(f, Finding{ELoopPass, at + ".pass", "loop needs a pass condition"})
		}
		if strings.TrimSpace(l.Focus) == "" {
			f = append(f, Finding{ELoopFocus, at + ".focus", "loop needs a focus"})
		}
		if len(l.Paths) == 0 {
			f = append(f, Finding{ENoPaths, at + ".paths", "loop must declare the paths the fixer owns"})
		}
	}
	return f
}

// Worker units own paths. An integrator unit owns the merge, not the files.
func needsPaths(role string) bool { return role == RoleWorker }

func countRole(minions []Minion, role string) int {
	n := 0
	for _, m := range minions {
		if m.Role == role {
			n++
		}
	}
	return n
}

// cycles reports one finding per distinct loop, in unit declaration order.
func cycles(units []Unit, byID map[string]Unit) []Finding {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var out []Finding
	seen := map[string]bool{}

	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range byID[id].Deps {
			if d == id {
				continue // the dep pass already named it
			}
			if _, ok := byID[d]; !ok {
				continue
			}
			switch color[d] {
			case white:
				visit(d)
			case grey:
				loop := loopFrom(stack, d)
				if key := loopKey(loop); !seen[key] {
					seen[key] = true
					out = append(out, Finding{
						ECycle, "units." + id + ".deps",
						"cycle: " + strings.Join(append(loop, d), " -> "),
					})
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}

	for _, u := range units {
		if color[u.ID] == white {
			visit(u.ID)
		}
	}
	return out
}

// loopFrom returns the nodes on the stack from the back edge's target to the
// node that closed the loop.
func loopFrom(stack []string, target string) []string {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == target {
			return append([]string(nil), stack[i:]...)
		}
	}
	return nil
}

// loopKey is rotation-invariant, so a->b->a and b->a->b are one loop.
func loopKey(loop []string) string {
	if len(loop) == 0 {
		return ""
	}
	min := 0
	for i := range loop {
		if loop[i] < loop[min] {
			min = i
		}
	}
	rotated := make([]string, 0, len(loop))
	for i := range loop {
		rotated = append(rotated, loop[(min+i)%len(loop)])
	}
	return strings.Join(rotated, ">")
}
