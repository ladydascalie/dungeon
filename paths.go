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
	return discoverPlansFrom(root)
}

// discoverPlansFrom is discoverPlans rooted at one .dungeon directory. The
// rail uses it for a pane whose cwd is not the rail's cwd.
func discoverPlansFrom(root string) []Encounter {
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
//
// Two prefixes are exempt, and only two: the encounter directory the plan lives
// in, and .dungeon/ at the top of the lane's own worktree, where a lane keeps
// screenshots and notes. A .dungeon directory anywhere deeper in the path is
// code: pkg/.dungeon/inject.go is a change outside the paths like any other, and
// a component found in the middle of a path exempts nothing.
func underEncounter(planPath, file string) bool {
	// git reports changed files relative to the directory it runs in, so the
	// worktree root .dungeon/ arrives as .dungeon/... or, from a subdirectory,
	// behind leading ../ segments. The exemption is that prefix, not any depth.
	parts := strings.Split(filepath.ToSlash(file), "/")
	i := 0
	for ; i < len(parts) && (parts[i] == "." || parts[i] == ".."); i++ {
	}
	if i < len(parts) && parts[i] == ".dungeon" {
		return true
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

// blindState names why git cannot be trusted to see this worktree: a path the
// index has been told not to look at, or a repo told to ignore the executable
// bit. A lane has bash, so it can set any of them on a tracked file outside its
// paths, modify the file, and settle on a tree that differs from the one land
// would keep. Nothing here fails open: a tree that cannot be read honestly is
// blocked, and the note names the file or the flag.
//
// The marks are read before the refresh, because a refresh clears the
// assume-unchanged bit it finds and reports the file as the change it is. That
// read is honest, but the lane set the flag to hide a file, and the operator
// should see the flag named rather than a change that looks ordinary.
//
// The refresh then runs before the caller reads the tree, so a stat cache that
// says nothing changed does not decide the read. A non-zero exit from it is not
// a failure: it reports the index it just refreshed, including unmerged
// entries, and the read that follows is what the guard judges.
func blindState(dir string) (string, error) {
	entries, err := gitNul(dir, "ls-files", "-v", "-z")
	if err != nil {
		return "", err
	}
	var marks []string
	for _, entry := range entries {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		tag, path := entry[0], entry[2:]
		switch {
		case tag == 'S':
			marks = append(marks, path+" is skip-worktree")
		case tag >= 'a' && tag <= 'z':
			marks = append(marks, path+" is assume-unchanged")
		}
	}
	if len(marks) > 0 {
		return "git is blind to part of this worktree: " + truncate(strings.Join(marks, ", "), 200) +
			" (clear it: git update-index --no-assume-unchanged <file>, --no-skip-worktree <file>)", nil
	}
	if mode, err := gitRun(dir, "config", "--bool", "core.fileMode"); err == nil && mode == "false" {
		return "git core.fileMode is false, so a mode change is invisible (git config core.fileMode true)", nil
	}
	_, _ = gitRun(dir, "update-index", "-q", "--really-refresh")
	return "", nil
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
// -uall lists an untracked tree file by file. Without it git reports one
// "dir/" entry, which no per-file path pattern can match, so a lane that
// created a directory was blocked for writing inside its own paths.
func changedFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain", "-z", "-uall")
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

// laneHead is the lane branch's commit when the unit is dispatched: the base
// the worktree guard counts the unit's changes from. A minion with no lane of
// its own has no head, and neither does a worktree that is not a git checkout,
// so nothing is recorded and the guard stays off for that unit.
func laneHead(p Plan, m Minion) string {
	if !writes(m.Role) {
		return ""
	}
	head, err := gitRun(minionDir(p, m), "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return head
}

// gitNul runs git and splits the NUL-terminated names it printed. It reads the
// bytes itself, so a name that begins or ends with a space survives: gitRun
// trims, and the first byte of the first name is then lost.
func gitNul(dir string, args ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	var names []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// changedSince lists the unit's changes: every file whose content differs
// between the recorded head and the tree now, plus every uncommitted file.
// Git compares trees, so the list is the same whether the lane committed the
// file, merged it, fast-forwarded to it, rebased onto it, cherry-picked it or
// left it in the worktree. --no-renames reports a rename as the deletion it is
// plus the addition it makes, so a deletion outside the paths cannot hide
// behind a destination inside them.
//
// It also returns the uncommitted subset: the files dungeon has left to commit.
func changedSince(dir, head string) (changed, uncommitted []string, err error) {
	if head != "" {
		if changed, err = gitNul(dir, "diff", "--name-only", "-z", "--no-renames", head); err != nil {
			return nil, nil, err
		}
	}
	if uncommitted, err = changedFiles(dir); err != nil {
		return nil, nil, err
	}
	all := map[string]bool{}
	for _, f := range changed {
		all[f] = true
	}
	for _, f := range uncommitted {
		all[f] = true
	}
	out := make([]string, 0, len(all))
	for f := range all {
		out = append(out, f)
	}
	sort.Strings(out)
	sort.Strings(uncommitted)
	return out, uncommitted, nil
}

// gitCheckout reports whether dir sits inside a git worktree. A pane with no
// checkout has no tree to contain, so containment cannot apply to it.
func gitCheckout(dir string) bool {
	out, err := gitRun(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// outsideNote is the block note for a unit whose tree holds files outside its
// paths. It carries the remedy, because a lane that merges a landed branch now
// hits this the normal way and the fix is a wider path, not a smaller merge.
func outsideNote(id string, outside []string) string {
	return "changed files outside your paths: " + truncate(strings.Join(outside, ", "), 100) +
		" (if you merged a landed branch, widen this unit's paths to cover it: " +
		"dungeon edit unit " + id + " --paths-add '<glob>')"
}

// commitLaneWork commits the named files on the branch the worktree has
// checked out. The parent does this, not the lane: a lane leaves its work in
// the worktree and never commits.
func commitLaneWork(dir, message string, files []string) error {
	args := append([]string{"add", "-A", "--"}, files...)
	if _, err := gitRun(dir, args...); err != nil {
		return err
	}
	if _, err := gitRun(dir, "commit", "-m", message, "--"); err != nil {
		return err
	}
	return nil
}

// blockedByWorktree is the containment check on a lane that writes. A unit
// owns its declared paths and nothing else, so every file whose content differs
// from the head recorded at dispatch must sit inside them. A change inside the
// paths is committed on the lane branch by dungeon, so the lane does not have to
// break the rule that lanes never commit. A change outside them means the split
// was not respected: it blocks and names the files and the remedy.
//
// The check asks what the tree holds, never which commit carried a file. A lane
// has bash in a git repo, so it can commit on a side branch and merge it, park a
// commit and fast-forward to it, rebase onto a landed dep, or hide a deletion in
// a rename. Those are the graph shapes an authorship question is answered from,
// and the lane can produce all of them. Containment asks it nothing.
//
// The count starts at the head recorded when the unit was dispatched, so a lane
// that commits its own work cannot hide it: git status goes empty and the unit
// would settle green whatever the lane touched. Diffing the branch against the
// plan's base instead would catch that commit and also every earlier unit on the
// same worktree, so the base is the unit's own dispatch head, not the plan's.
//
// A unit whose state carries no recorded head, which is every unit dispatched
// before this guard landed and every pane with no lane of its own, is judged by
// git status alone, exactly as it was, and only where git can read a checkout.
// The guard is new, not retroactive.
//
// A git failure on a unit with a recorded head blocks. A check that cannot run
// has not shown that the unit contains its work, and failing open is the one
// failure a lane could aim for.
//
// The tree is read only when git can see it honestly. An index that carries an
// assume-unchanged or skip-worktree entry, or a repo with core.fileMode off,
// hides a tracked file from status and from the diff, so the accept gate would
// run against a tree that is not the tree land merges. That blocks too, and the
// note names the file or the flag.
func blockedByWorktree(o dispatchOpts, p Plan, st *State, u Unit, m Minion) (bool, string) {
	dir, head := minionDir(p, m), st.Units[u.ID].LaneHead
	if head == "" && !gitCheckout(dir) {
		return false, ""
	}
	if note, err := blindState(dir); err != nil {
		st.SetUnit(u.ID, "blocked", "could not read the unit's worktree: "+truncate(err.Error(), 200))
		return true, ""
	} else if note != "" {
		st.SetUnit(u.ID, "blocked", note)
		return true, ""
	}
	changed, uncommitted, err := changedSince(dir, head)
	if err != nil {
		st.SetUnit(u.ID, "blocked", "could not check the unit's changes: "+truncate(err.Error(), 200))
		return true, ""
	}
	pending := map[string]bool{}
	for _, f := range uncommitted {
		pending[f] = true
	}
	// Committed work is already on the branch. Uncommitted work is what dungeon
	// still has to commit, and it stays uncommitted when both lists hold it.
	var outside, committed, mine []string
	for _, f := range changed {
		if underEncounter(o.planPath, f) {
			continue
		}
		switch {
		case !ownedBy(u.Paths, f):
			outside = append(outside, f)
		case pending[f]:
			mine = append(mine, f)
		default:
			committed = append(committed, f)
		}
	}
	if len(outside) > 0 {
		st.SetUnit(u.ID, "blocked", outsideNote(u.ID, outside))
		return true, ""
	}
	note := ""
	if len(committed) > 0 && writes(m.Role) {
		note = "work already committed in your paths: " + truncate(strings.Join(committed, " "), 100)
	}
	switch {
	case len(mine) == 0:
		return false, note
	case !writes(m.Role):
		st.SetUnit(u.ID, "blocked", "uncommitted work in "+truncate(strings.Join(mine, " "), 100))
		return true, ""
	}
	message := "dungeon: commit lane work for unit " + u.ID
	if err := commitLaneWork(dir, message, mine); err != nil {
		st.SetUnit(u.ID, "blocked", "could not commit work in "+
			truncate(strings.Join(mine, " "), 100)+": "+err.Error())
		return true, ""
	}
	return false, note
}
