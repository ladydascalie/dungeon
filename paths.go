package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
// its root. A multiplexer pane or a subdirectory of the repo then needs no flag.
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
// default plan.yaml and one plan.yaml per subdirectory, newest touch first so the
// picker opens on the plan the operator last worked. Extra encounters live in
// their own fold, so each keeps its own state, briefs and reports.
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
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := lastTouched(out[i].Path), lastTouched(out[j].Path)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// lastTouched is when the encounter last moved: the newest of its plan and its
// state, so a running encounter outranks one that was only edited once.
func lastTouched(planPath string) time.Time {
	var newest time.Time
	for _, path := range []string{planPath, StatePath(planPath)} {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
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

// changedFiles lists what git sees as changed in dir. It reads porcelain with
// -z and parses the NUL entries itself: a leading space is the index status of
// a modified file, so trimming the output loses a byte and corrupts the path.
func changedFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain -z in %s: %w", dir, err)
	}
	return parsePorcelainZ(out), nil
}

// parsePorcelainZ reads the NUL-separated entries of git status --porcelain -z.
// Each entry is "XY path"; a rename or copy carries its source as the next
// entry, so the destination is kept and the source skipped.
func parsePorcelainZ(out []byte) []string {
	entries := strings.Split(string(out), "\x00")
	var files []string
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue // end of output, or not a status entry
		}
		status, path := entry[:2], entry[3:]
		if strings.ContainsAny(status, "RC") {
			i++ // the source of a rename or copy follows as its own entry
		}
		files = append(files, path)
	}
	return files
}

// commitLaneWork commits the named files on the branch the worktree has
// checked out. The parent does this, not the lane: a lane leaves its work in
// the worktree and never commits.
func commitLaneWork(dir, message string, files []string) error {
	args := append([]string{"add", "--"}, files...)
	if _, err := gitRun(dir, args...); err != nil {
		return err
	}
	if _, err := gitRun(dir, "commit", "-m", message, "--"); err != nil {
		return err
	}
	return nil
}

// blockedByWorktree is the honesty check on a lane that writes. Changes inside
// the paths a writing unit owns are committed on the lane branch by dungeon, so
// the lane does not have to break the rule that lanes never commit. A change
// outside those paths means the split was not respected: it blocks and names
// the files. A non-writing lane has no branch of its own, so its uncommitted
// work still blocks.
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
	case len(outside) > 0:
		st.SetUnit(u.ID, "blocked", "changed files outside your paths: "+
			truncate(strings.Join(outside, " "), 100))
		return true
	case len(mine) == 0:
		return false
	case !writes(m.Role):
		st.SetUnit(u.ID, "blocked", "uncommitted work in "+truncate(strings.Join(mine, " "), 100))
		return true
	}
	message := "dungeon: commit lane work for unit " + u.ID
	if err := commitLaneWork(minionDir(p, m), message, mine); err != nil {
		st.SetUnit(u.ID, "blocked", "could not commit work in "+
			truncate(strings.Join(mine, " "), 100)+": "+err.Error())
		return true
	}
	return false
}
