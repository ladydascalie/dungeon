package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Codes are stable: a plan's problems are addressed by code, not by prose.
type Code string

const (
	EParse            Code = "E_PARSE"
	ENoEncounter      Code = "E_NO_ENCOUNTER"
	EBadEncounter     Code = "E_BAD_ENCOUNTER"
	ENoMinion         Code = "E_NO_MINION"
	EUnknownMinion    Code = "E_UNKNOWN_MINION"
	EUnknownRole      Code = "E_UNKNOWN_ROLE"
	EDuplicateID      Code = "E_DUPLICATE_ID"
	EDuplicatePane    Code = "E_DUPLICATE_PANE"
	EDuplicateAgent   Code = "E_DUPLICATE_AGENT"
	EBadMinionID      Code = "E_BAD_MINION_ID"
	ENoID             Code = "E_NO_ID"
	ENoWorktree       Code = "E_NO_WORKTREE"
	EDMPlaysMinion    Code = "E_DM_PLAYS_MINION"
	ENoPaths          Code = "E_NO_PATHS"
	EMissingDep       Code = "E_MISSING_DEP"
	ECycle            Code = "E_CYCLE"
	EDepPlan          Code = "E_DEP_PLAN"
	EDepUnit          Code = "E_DEP_UNIT"
	EDepSelf          Code = "E_DEP_SELF"
	ESplitInvalid     Code = "E_SPLIT_INVALID"
	ESplitNoMinion    Code = "E_SPLIT_NO_MINION"
	EUnknownSplit     Code = "E_UNKNOWN_SPLIT"
	EBadTools         Code = "E_BAD_TOOLS"
	EVerdictRole      Code = "E_VERDICT_ROLE"
	EVerdictAccept    Code = "E_VERDICT_ACCEPT"
	EIntegratorAccept Code = "E_INTEGRATOR_ACCEPT"
	ELoopRole         Code = "E_LOOP_ROLE"
	ELoopRounds       Code = "E_LOOP_ROUNDS"
	ELoopPass         Code = "E_LOOP_PASS"
	ELoopFocus        Code = "E_LOOP_FOCUS"
	EOfNotWriter      Code = "E_OF_NOT_WRITER"
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

// A minion id becomes an agent name, so it obeys dungeon's own naming rule:
// lowercase, and short enough for every backend to accept.
var minionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`) // 32 characters: fits every backend

// A tool name is what pi accepts after --tools.
var toolName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// The directions a pane can be split in.
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

		if role == RoleIntegrator && u.Accept != "" {
			f = append(f, Finding{
				EIntegratorAccept, at + ".accept",
				fmt.Sprintf("integrator unit %q cannot set accept: its worktree is cut from base and does not contain the other writers' work. Drop the accept; dungeon land merges and re-gates", u.ID),
			})
		}

		for j, d := range u.Deps {
			if !d.Local() {
				// A cross-plan dep is ValidateDeps's job: this pass sees one plan.
				continue
			}
			where := fmt.Sprintf("%s.deps[%d]", at, j)
			if d.Unit == u.ID {
				f = append(f, Finding{ECycle, where, "unit depends on itself"})
				continue
			}
			if !unitIDs[d.Unit] {
				f = append(f, Finding{
					EMissingDep, where,
					fmt.Sprintf("dep %q is not a declared unit", d.Unit),
				})
			}
		}
	}

	f = append(f, checkLoops(p.Loops, roleOf, unitIDs)...)
	f = append(f, duplicatePanes(p.Minions)...)
	f = append(f, duplicateAgentNames(p.Minions)...)
	return append(f, cycles(p.Units, rep)...)
}

// resolveDepPlan is the plan file a dep names. An absolute plan is used as it
// is. A relative one resolves against the plan that names it, because a
// committed plan should carry a relative path.
func resolveDepPlan(planPath string, d Dep) string {
	if filepath.IsAbs(d.Plan) {
		return filepath.Clean(d.Plan)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(planPath), d.Plan))
}

// absPath is the key a plan is cached and compared by: one file, one key.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// depPlan is another plan as a dep sees it: decoded and expanded, so a dep may
// name a generated loop unit such as auth-review-3.
type depPlan struct {
	plan     Plan
	findings []Finding
	err      error
}

// depPlans decodes each plan once, however many deps name it.
type depPlans map[string]depPlan

func (c depPlans) get(path string) depPlan {
	if d, ok := c[path]; ok {
		return d
	}
	var d depPlan
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		d.err = err
	default:
		p, f := Decode(data)
		if len(f) > 0 {
			d.findings = f
			break
		}
		d.plan, _ = ExpandLoops(p)
	}
	c[path] = d
	return d
}

// ValidateDeps checks the deps that leave the plan. Validate checks a local dep
// against the units it can see; a cross-plan dep is checked against the plan it
// names, and the cycle that crosses plans is caught here because cycles() sees
// one plan.
func ValidateDeps(planPath string, p Plan) []Finding {
	var f []Finding
	self := absPath(planPath)
	cache := depPlans{}
	local, _ := ExpandLoops(p)
	cache[self] = depPlan{plan: local}

	for i, u := range p.Units {
		for j, d := range u.Deps {
			if d.Local() {
				continue
			}
			where := fmt.Sprintf("units[%d].deps[%d]", i, j)
			path := absPath(resolveDepPlan(planPath, d))
			if path == self {
				f = append(f, Finding{
					EDepSelf, where,
					fmt.Sprintf("dep %s names this plan's own file: write %q as a local dep", d, d.Unit),
				})
				continue
			}
			other := cache.get(path)
			switch {
			case other.err != nil:
				f = append(f, Finding{
					EDepPlan, where,
					fmt.Sprintf("dep %s: cannot read %s: %v", d, path, other.err),
				})
			case len(other.findings) > 0:
				f = append(f, Finding{
					EDepPlan, where,
					fmt.Sprintf("dep %s: %s is not a valid plan: %s", d, path, other.findings[0]),
				})
			default:
				if _, ok := findUnit(other.plan, d.Unit); !ok {
					f = append(f, Finding{
						EDepUnit, where,
						fmt.Sprintf("dep %s: %s declares no unit %q", d, path, d.Unit),
					})
				}
			}
		}
	}
	return append(f, crossPlanCycles(self, local, cache)...)
}

// crossDepthCap bounds the walk. A chain deeper than this is not a plan, and
// stopping keeps a runaway dep graph from hanging check.
const crossDepthCap = 8

// crossPlanCycles reports a cycle that leaves the plan and comes back. Each
// distinct chain is reported once.
//
// The walk is memoised by (plan, unit, remaining depth): a node is walked once
// per remaining budget, so the cost stays bounded by the cap times the graph
// however many paths reach it. A node found on the stack is a back edge: its
// cycle is reported and the node is not walked, because its own frame is still
// exploring it with a larger budget. Nothing is memoised for a back edge: only
// the frame that owns the node records a result, once its subtree is walked.
func crossPlanCycles(self string, p Plan, cache depPlans) []Finding {
	type node struct{ key, label string }
	type visitKey struct {
		key       string
		remaining int
	}
	var out []Finding
	reported := map[string]bool{}
	done := map[visitKey]bool{}
	var stack []node

	var visit func(planPath, unit string, remaining int)
	visit = func(planPath, unit string, remaining int) {
		if remaining < 0 {
			return
		}
		key := planPath + "|" + unit
		at := visitKey{key, remaining}
		if done[at] {
			return
		}
		label := unit
		if planPath != self {
			label = planPath + "#" + unit
		}
		for i, n := range stack {
			if n.key != key {
				continue
			}
			chain := make([]string, 0, len(stack)-i+1)
			for _, m := range stack[i:] {
				chain = append(chain, m.label)
			}
			chain = append(chain, label)
			if k := loopKey(chain); !reported[k] {
				reported[k] = true
				out = append(out, Finding{
					ECycle, "units." + unit + ".deps",
					"cycle across plans: " + strings.Join(chain, " -> "),
				})
			}
			return
		}
		stack = append(stack, node{key, label})
		if u, ok := findUnit(cache.get(planPath).plan, unit); ok {
			for _, d := range u.Deps {
				next := planPath
				if !d.Local() {
					next = absPath(resolveDepPlan(planPath, d))
					if next == planPath {
						continue // a dep on the plan's own file is E_DEP_SELF's finding
					}
				}
				visit(next, d.Unit, remaining-1)
			}
		}
		stack = stack[:len(stack)-1]
		done[at] = true
	}

	for _, u := range p.Units {
		if hasCrossDep(u) {
			visit(self, u.ID, crossDepthCap)
		}
	}
	return out
}

func hasCrossDep(u Unit) bool {
	for _, d := range u.Deps {
		if !d.Local() {
			return true
		}
	}
	return false
}

// duplicateAgentNames reports minion ids whose derived agent names collide
// after truncation. The token is the same for every minion in one run, so only
// a truncated id can make two names equal.
func duplicateAgentNames(minions []Minion) []Finding {
	var f []Finding
	first := map[string]string{}
	for i, m := range minions {
		if m.ID == "" {
			continue
		}
		name := AgentName("00", m.ID)
		if other, ok := first[name]; ok {
			f = append(f, Finding{
				EDuplicateAgent, fmt.Sprintf("minions[%d].id", i),
				fmt.Sprintf("minions %q and %q both derive the agent name %q: shorten one id",
					other, m.ID, name),
			})
			continue
		}
		first[name] = m.ID
	}
	return f
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
		for j, d := range l.Deps {
			if !unitIDs[d] {
				f = append(f, Finding{
					EMissingDep, fmt.Sprintf("%s.deps[%d]", at, j),
					fmt.Sprintf("dep %q is not a declared unit", d),
				})
			}
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
		for _, dep := range byID[id].Deps {
			if !dep.Local() {
				continue // a cycle across plans is ValidateDeps's job
			}
			d := dep.Unit
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
