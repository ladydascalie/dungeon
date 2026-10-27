package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// init writes the smallest plan that validates and explains itself. A plan is
// the one thing the tool cannot derive: it has to come from a person who knows
// what the work is.

const initGitignore = `state.json
sessions/
briefs/
reports/
*.tmp
`

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func initPlan(planPath, encounter, base string, force bool) error {
	if _, err := os.Stat(planPath); err == nil && !force {
		return fmt.Errorf("%s already exists. Edit it, or pass --force to replace it", planPath)
	}
	dir := filepath.Dir(planPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	repo := gitOutput(filepath.Dir(dir), "rev-parse", "--show-toplevel")
	if repo == "" {
		repo = filepath.Dir(dir)
	}
	if base == "" {
		base = gitOutput(repo, "symbolic-ref", "--short", "HEAD")
	}
	if base == "" {
		base = "main"
	}
	// An encounter becomes a branch prefix, so it is always slugged, never kept
	// as typed.
	encounter = slug(encounter)
	if encounter == "" {
		encounter = slug(filepath.Base(repo))
	}
	if encounter == "" {
		encounter = "encounter"
	}

	plan := fmt.Sprintf(`# dungeon plan. Edit this, then:
#   dungeon check          # validates
#   dungeon up             # spawns the lanes and dispatches
#   dungeon view           # the live graph
encounter: %s
repo: %s
base: %s
concurrency: 1

minions:
  # A writer owns a worktree, may edit files, and must declare the paths it owns.
  - {id: w1, role: worker, worktree: w1}
  # A reviewer reads and returns a verdict. It owns no paths and gets no worktree.
  # - {id: r1, role: reviewer, of: w1}

# One unit is one reviewable change: one concern, one minion, one acceptance
# command. Uncomment the block below and drop the empty list.
#
# units:
#   - id: first-change
#     minion: w1
#     paths: [src/**]
#     deps: []
#     goal: What changes, in one sentence.
#     accept: make test
units: []
`, encounter, repo, base)

	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		return err
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); os.IsNotExist(err) {
		if err := os.WriteFile(ignore, []byte(initGitignore), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func initCmd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	dir := fs.String("dir", ".", "directory to hold .dungeon")
	planPath := fs.String("plan", "", "path to write (default: <dir>/.dungeon/plan.yaml)")
	encounter := fs.String("encounter", "", "encounter name (default: the repo directory name)")
	base := fs.String("base", "", "branch to cut worktrees from (default: the current branch)")
	force := fs.Bool("force", false, "replace an existing plan")
	rest, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(rest) > 0 {
		*dir = rest[0]
	}
	if *planPath == "" {
		*planPath = filepath.Join(*dir, ".dungeon", "plan.yaml")
	}

	if err := initPlan(*planPath, *encounter, *base, *force); err != nil {
		fmt.Fprintf(stderr, "dungeon: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "wrote %s\n\nnext:\n  edit it\n  dungeon check %s\n  dungeon up %s\n",
		*planPath, *planPath, *planPath)
	return 0
}
