package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWritesAPlanThatValidates(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, ".dungeon", "plan.yaml")

	code, stdout, stderr := runCLI("init", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dungeon check") {
		t.Fatalf("init should say what to run next: %q", stdout)
	}

	p, findings, err := DecodeFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) > 0 {
		t.Fatalf("the starter plan must decode: %v", findings)
	}
	if got := Validate(p); len(got) > 0 {
		t.Fatalf("the starter plan must validate: %v", got)
	}
	if p.Encounter == "" || p.Repo == "" || p.Base == "" {
		t.Fatalf("starter plan is missing identity: %+v", p)
	}
	if len(p.Minions) != 1 || p.Minions[0].Role != "worker" {
		t.Fatalf("starter should hold one worker, got %+v", p.Minions)
	}
}

func TestInitDetectsRepoAndBranch(t *testing.T) {
	repo := gitRepo(t) // initialised on main
	code, _, stderr := runCLI("init", "--dir", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	p, _, err := DecodeFile(filepath.Join(repo, ".dungeon", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != "main" {
		t.Fatalf("base %q, want main", p.Base)
	}
	if p.Repo != repo {
		t.Fatalf("repo %q, want %q", p.Repo, repo)
	}
	if p.Encounter != slug(filepath.Base(repo)) {
		t.Fatalf("encounter %q, want the repo name", p.Encounter)
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI("init", "--dir", dir); code != 0 {
		t.Fatalf("first init failed")
	}
	code, _, stderr := runCLI("init", "--dir", dir)
	if code != 2 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("exit %d: %q", code, stderr)
	}
	if code, _, _ := runCLI("init", "--dir", dir, "--force"); code != 0 {
		t.Fatalf("--force should replace the plan")
	}
}

func TestInitKeepsTheRunOutOfGit(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI("init", "--dir", dir); code != 0 {
		t.Fatal("init failed")
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".dungeon", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state.json", "briefs/", "reports/", "sessions/"} {
		if !strings.Contains(string(ignore), want) {
			t.Fatalf(".dungeon/.gitignore should ignore %q:\n%s", want, ignore)
		}
	}
	if strings.Contains(string(ignore), "plan.yaml") {
		t.Fatal("the plan belongs in git, it must not be ignored")
	}
}

func TestInitEncounterOverride(t *testing.T) {
	dir := t.TempDir()
	if code, _, stderr := runCLI("init", "--dir", dir, "--encounter", "My Thing"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	p, _, err := DecodeFile(filepath.Join(dir, ".dungeon", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Encounter != "my-thing" {
		t.Fatalf("encounter %q, want my-thing: it becomes a branch prefix", p.Encounter)
	}
}
