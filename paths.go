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

// laneCommit is one commit on the lane's own first-parent chain from HEAD back
// to the recorded head, with the short name a note points at.
type laneCommit struct {
	full  string
	short string
}

// laneCommits lists the commits the lane made between head and HEAD: the
// first-parent chain from HEAD back to head, newest first. A merge brings the
// other branch's commits into the range, but they are not on this chain, so
// they never count as the lane's own. An empty head lists nothing: it is every
// unit dispatched before the guard landed.
func laneCommits(dir, head string) ([]laneCommit, error) {
	if head == "" {
		return nil, nil
	}
	out, err := gitRun(dir, "log", "--first-parent", "--format=%H %h", head+"..HEAD")
	if err != nil {
		return nil, err
	}
	var commits []laneCommit
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		commits = append(commits, laneCommit{full: fields[0], short: fields[1]})
	}
	return commits, nil
}

// commitFiles lists the files a commit wrote. A normal commit writes what
// differs from its parent. A merge would also name everything the other side
// brought, which the lane did not write, so it is narrowed to the files that
// differ from every parent: the resolution work and nothing else.
func commitFiles(dir string, c laneCommit) ([]string, error) {
	mine, err := gitNul(dir, "diff", "--name-only", "-z", c.full+"^1", c.full)
	if err != nil {
		return nil, err
	}
	if _, err := gitRun(dir, "rev-parse", "--verify", "--quiet", c.full+"^2"); err != nil {
		return mine, nil // one parent: a normal commit wrote all of it
	}
	theirs, err := gitNul(dir, "diff", "--name-only", "-z", c.full+"^2", c.full)
	if err != nil {
		return nil, err
	}
	both := toSet(theirs)
	var out []string
	for _, f := range mine {
		if both[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// arrivedCommits lists the commits a fast-forward brought onto the lane branch
// between head and HEAD. A fast-forward creates no commit and leaves no trace in
// the graph, so those commits would read as the lane's own work. The branch
// reflog records the move and the state it moved from, which names them.
//
// A reflog that cannot be read returns nothing: attribution then falls back to
// the graph, which is all there is.
func arrivedCommits(dir, head string) map[string]bool {
	if head == "" {
		return nil
	}
	out, err := gitRun(dir, "log", "-g", "--format=%H %gs", "HEAD")
	if err != nil {
		return nil
	}
	type entry struct{ hash, subject string }
	var entries []entry
	for _, line := range strings.Split(out, "\n") {
		hash, subject, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		entries = append(entries, entry{hash: hash, subject: subject})
	}
	arrived := map[string]bool{}
	for i, e := range entries {
		// The recorded head is where this unit's window starts: older moves
		// are another unit's business.
		if e.hash == head {
			break
		}
		if i+1 >= len(entries) || !strings.Contains(e.subject, "Fast-forward") {
			continue
		}
		brought, err := gitRun(dir, "rev-list", "--first-parent", entries[i+1].hash+".."+e.hash)
		if err != nil {
			continue
		}
		for _, c := range strings.Fields(brought) {
			arrived[c] = true
		}
	}
	return arrived
}

// writtenFiles maps every file the lane wrote between head and HEAD to the short
// commit that wrote it, newest commit wins. A file that only arrived, through a
// merge or a fast-forward, is absent: the unit did not write it, so no note may
// claim it did and no path may call it outside.
func writtenFiles(dir, head string) (map[string]string, error) {
	if head == "" {
		return nil, nil
	}
	commits, err := laneCommits(dir, head)
	if err != nil {
		return nil, err
	}
	arrived := arrivedCommits(dir, head)
	written := map[string]string{}
	for _, c := range commits {
		if arrived[c.full] {
			continue
		}
		files, err := commitFiles(dir, c)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if _, ok := written[f]; !ok {
				written[f] = c.short
			}
		}
	}
	return written, nil
}

// writtenNames lists the files of a written set in a stable order, so one note
// reads the same on every run.
func writtenNames(written map[string]string) []string {
	out := make([]string, 0, len(written))
	for f := range written {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// filesWithCommits names the files, and for one the lane wrote, the short commit
// that wrote it. An uncommitted file is named alone: no commit carried it.
func filesWithCommits(written map[string]string, files []string) string {
	tokens := make([]string, 0, len(files))
	for _, f := range files {
		if c := written[f]; c != "" {
			tokens = append(tokens, f+" in "+c)
			continue
		}
		tokens = append(tokens, f)
	}
	return strings.Join(tokens, " ")
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

// blockedByWorktree is the honesty check on a lane that writes. Changes inside
// the paths a writing unit owns are committed on the lane branch by dungeon, so
// the lane does not have to break the rule that lanes never commit. A change
// outside those paths means the split was not respected: it blocks and names
// the files. A non-writing lane has no branch of its own, so its uncommitted
// work still blocks.
//
// The count starts at the head recorded when the unit was dispatched, so a lane
// that commits its own work cannot hide it: git status goes empty and the unit
// would settle green whatever the lane touched. Diffing the branch against the
// plan's base instead would catch that commit and also every earlier unit on the
// same worktree, so the base is the unit's own dispatch head, not the plan's.
//
// Committed work is attributed per file, not by subtracting one tree from
// another: a file counts only when a commit the lane made is what changed it. A
// lane told to merge a landed branch then keeps the dep's files out of its
// account, because a merge does not write them; only its own resolution is its
// work. A cherry-pick is the lane's own commit by the same rule, and it blocks
// when the file is outside the paths.
//
// A unit whose state carries no recorded head, which is every unit dispatched
// before this guard landed, is judged by git status alone, exactly as it was.
// The guard is new, not retroactive.
//
// The returned note says what the lane committed by itself, for the settle note.
func blockedByWorktree(o dispatchOpts, p Plan, st *State, u Unit, m Minion) (bool, string) {
	dir, head := minionDir(p, m), st.Units[u.ID].LaneHead
	uncommitted, err := changedFiles(dir)
	if err != nil {
		return false, ""
	}
	written, err := writtenFiles(dir, head)
	if err != nil {
		return false, ""
	}
	keep := func(files []string) []string {
		var out []string
		for _, f := range files {
			if !underEncounter(o.planPath, f) {
				out = append(out, f)
			}
		}
		return out
	}
	// Written work is the lane's own; uncommitted work is what dungeon commits.
	// A file in both lists is still uncommitted, so it is committed.
	var mineWritten, mineUncommitted, outside []string
	for _, f := range keep(writtenNames(written)) {
		if ownedBy(u.Paths, f) {
			mineWritten = append(mineWritten, f)
		} else {
			outside = append(outside, f)
		}
	}
	for _, f := range keep(uncommitted) {
		if ownedBy(u.Paths, f) {
			mineUncommitted = append(mineUncommitted, f)
		} else if _, wrote := written[f]; !wrote {
			outside = append(outside, f)
		}
	}
	note := ""
	if len(mineWritten) > 0 && writes(m.Role) {
		note = "lane committed its own work: " + truncate(filesWithCommits(written, mineWritten), 100)
	}
	switch {
	case len(outside) > 0:
		st.SetUnit(u.ID, "blocked", "changed files outside your paths: "+
			truncate(filesWithCommits(written, outside), 100))
		return true, ""
	case len(mineUncommitted) == 0:
		return false, note
	case !writes(m.Role):
		st.SetUnit(u.ID, "blocked", "uncommitted work in "+truncate(strings.Join(mineUncommitted, " "), 100))
		return true, ""
	}
	message := "dungeon: commit lane work for unit " + u.ID
	if err := commitLaneWork(dir, message, mineUncommitted); err != nil {
		st.SetUnit(u.ID, "blocked", "could not commit work in "+
			truncate(strings.Join(mineUncommitted, " "), 100)+": "+err.Error())
		return true, ""
	}
	return false, note
}
