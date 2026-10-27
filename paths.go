package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// encounterDir is absolute, always. A lane runs in a worktree where a relative
// .dungeon/ would be created inside the checkout, so nothing handed to a lane
// may be relative.
func encounterDir(planPath string) string {
	if abs, err := filepath.Abs(filepath.Dir(planPath)); err == nil {
		return abs
	}
	return filepath.Dir(planPath)
}

// defaultPlan finds the encounter from wherever the command runs: the plan is
// .dungeon/plan.yaml, searched up from the working directory the way git finds
// its root. A herdr pane or a subdirectory of the repo then needs no flag.
func defaultPlan() string {
	dir, err := os.Getwd()
	if err != nil {
		return defaultPlanPath
	}
	for {
		candidate := filepath.Join(dir, ".dungeon", "plan.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return defaultPlanPath
		}
		dir = parent
	}
}

// Encounter is one discoverable plan under a .dungeon directory. A plan that
// does not parse is still listed, with Err, so the operator can see it.
type Encounter struct {
	Path string
	Name string
	Err  error
}

// encounterRoot finds the nearest .dungeon directory, the way defaultPlan finds
// the nearest plan.
func encounterRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(dir, ".dungeon")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// discoverPlans lists every encounter under the nearest .dungeon directory: the
// default plan.yaml first, then one plan.yaml per subdirectory. Extra encounters
// live in their own fold, so each keeps its own state, briefs and reports.
func discoverPlans() []Encounter {
	root, ok := encounterRoot()
	if !ok {
		return nil
	}
	var out []Encounter
	add := func(path string) {
		if _, err := os.Stat(path); err != nil {
			return
		}
		e := Encounter{Path: path}
		p, findings, err := DecodeFile(path)
		switch {
		case err != nil:
			e.Err = err
		case len(findings) > 0:
			e.Err = fmt.Errorf("%s", findings[0])
		}
		e.Name = p.Encounter
		if e.Name == "" {
			e.Name = filepath.Base(filepath.Dir(path))
		}
		out = append(out, e)
	}
	add(filepath.Join(root, "plan.yaml"))
	plans, _ := filepath.Glob(filepath.Join(root, "*", "plan.yaml"))
	sort.Strings(plans)
	for _, path := range plans {
		add(path)
	}
	return out
}

// shortPath names a plan relative to the working directory when it can, so a
// refusal and the picker read like the operator's shell.
func shortPath(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil {
		return path
	}
	return rel
}

// printEncounters lists plans one per line: path first, encounter name second.
// Both the refusal and the picker read the same list.
func printEncounters(w io.Writer, found []Encounter) {
	width := 0
	for _, e := range found {
		width = max(width, runeLen(shortPath(e.Path)))
	}
	for _, e := range found {
		name := e.Name
		if e.Err != nil {
			name = "unreadable"
		}
		fmt.Fprintf(w, "  %s  %s\n", pad(shortPath(e.Path), width), name)
	}
}

// underEncounter reports whether a path belongs to the encounter rather than to
// the code: the state, the briefs, the reports, the lane sessions.
func underEncounter(planPath, file string) bool {
	// git reports changed files relative to the directory it runs in, so a
	// lane's own .dungeon/ never matches the plan's encounter by subtraction.
	// Every .dungeon component is encounter scratch: the state, a brief, a
	// report, a screenshot.
	for _, part := range strings.Split(filepath.ToSlash(file), "/") {
		if part == ".dungeon" {
			return true
		}
	}
	if !filepath.IsAbs(file) {
		return false
	}
	rel, err := filepath.Rel(encounterDir(planPath), file)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ownedBy reports whether a changed file sits inside the paths a unit declared.
// A trailing /** or /* means the subtree.
func ownedBy(patterns []string, file string) bool {
	for _, p := range patterns {
		p = strings.TrimSuffix(strings.TrimSuffix(p, "/**"), "/*")
		if file == p || strings.HasPrefix(file, p+"/") {
			return true
		}
		if ok, _ := filepath.Match(p, file); ok {
			return true
		}
	}
	return false
}

// changedFiles lists what git sees as changed in dir.
func changedFiles(dir string) ([]string, error) {
	out, err := gitRun(dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.LastIndex(path, " -> "); i >= 0 {
			path = path[i+4:] // a rename reports both ends; the new one is the file
		}
		files = append(files, strings.Trim(path, `"`))
	}
	return files, nil
}

// blockedByWorktree is the honesty check on a lane that writes. Uncommitted work
// in the paths it owns means nothing to land. A change outside those paths means
// the split was not respected. Both block the unit and say which one happened.
func blockedByWorktree(o dispatchOpts, p Plan, st *State, u Unit, m Minion) bool {
	files, err := changedFiles(minionDir(p, m))
	if err != nil {
		return false
	}
	var mine, outside []string
	for _, f := range files {
		switch {
		case underEncounter(o.planPath, f):
			// the state, a brief, a session: not code, and not the lane's work
		case ownedBy(u.Paths, f):
			mine = append(mine, f)
		default:
			outside = append(outside, f)
		}
	}
	switch {
	case len(mine) > 0:
		st.SetUnit(u.ID, "blocked", "uncommitted work in "+truncate(strings.Join(mine, " "), 100)+
			": commit it on your branch, then run dungeon next")
	case len(outside) > 0:
		st.SetUnit(u.ID, "blocked", "changed files outside your paths: "+
			truncate(strings.Join(outside, " "), 100))
	default:
		return false
	}
	return true
}
