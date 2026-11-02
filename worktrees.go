package main

import (
	"encoding/json"
	"os/exec"
	"strings"
)

// A worktree can move after it is cut. The convention in worktreePath is where
// dungeon creates a lane; it is not proof of where the lane is now. Ask
// worktrunk, then git, for the path that has the branch checked out.

type wtList struct {
	Items []struct {
		Branch   string `json:"branch"`
		Worktree *struct {
			Path string `json:"path"`
		} `json:"worktree"`
	} `json:"items"`
}

// resolveWorktreePath returns the path that has branch checked out. It reports
// false when no worktree has the branch, so a caller can fall back to the
// creation convention.
func resolveWorktreePath(repo, branch string) (string, bool) {
	if path, found, ok := wtWorktreePath(repo, branch); ok {
		return path, found
	}
	return gitWorktreePath(repo, branch)
}

// wtWorktreePath asks worktrunk when it is installed. ok is false when
// worktrunk could not answer, so the caller can ask git instead.
func wtWorktreePath(repo, branch string) (path string, found bool, ok bool) {
	if _, err := exec.LookPath("wt"); err != nil {
		return "", false, false
	}
	out, err := exec.Command("wt", "-C", repo, "list", "--format", "json").Output()
	if err != nil {
		return "", false, false
	}
	var list wtList
	if err := json.Unmarshal(out, &list); err != nil {
		return "", false, false
	}
	for _, item := range list.Items {
		if item.Branch == branch && item.Worktree != nil && item.Worktree.Path != "" {
			return item.Worktree.Path, true, true
		}
	}
	return "", false, true
}

// gitWorktreePath is the fallback, and the only source when wt is missing.
func gitWorktreePath(repo, branch string) (string, bool) {
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", false
	}
	var path string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			if path != "" {
				return path, true
			}
		case line == "":
			path = ""
		}
	}
	return "", false
}
