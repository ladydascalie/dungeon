package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPlanIsSearchedUpwards(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".dungeon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(plan, []byte("encounter: x\nminions: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(deep); err != nil {
		t.Fatal(err)
	}
	if got := defaultPlan(); got != plan {
		t.Fatalf("got %q, want %q", got, plan)
	}
}

func TestDefaultPlanFallsBackWhenThereIsNone(t *testing.T) {
	empty := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	// /tmp has no ancestor plan in a normal checkout, but the fallback must still
	// be the relative default rather than an error.
	if got := defaultPlan(); got == "" {
		t.Fatal("want a path, got empty")
	}
}
