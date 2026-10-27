package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkRecordsAForgeAgnosticURL(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")

	code, stdout, stderr := runCLI("link", "A", "https://gitlab.example.com/g/p/-/merge_requests/7",
		"--plan", plan, "--state", state)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "merge_requests/7") {
		t.Fatalf("output %q", stdout)
	}
	st, _ := LoadState(state)
	if got := st.Units["A"].Link; !strings.Contains(got, "gitlab.example.com") {
		t.Fatalf("link %q", got)
	}
}

func TestLinkRefusesAnythingButHTTP(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "not a url", "ftp://x/y"} {
		code, _, _ := runCLI("link", "A", bad, "--plan", plan, "--state", state)
		if code != 2 {
			t.Fatalf("%q: exit %d, want 2", bad, code)
		}
	}
	if code, _, _ := runCLI("link", "nope", "https://x/y", "--plan", plan, "--state", state); code != 2 {
		t.Fatalf("unknown unit: exit %d, want 2", code)
	}
}

func TestLinkClear(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if code, _, _ := runCLI("link", "A", "https://x/y", "--plan", plan, "--state", state); code != 0 {
		t.Fatal("link failed")
	}
	code, stdout, _ := runCLI("link", "A", "--clear", "--plan", plan, "--state", state)
	if code != 0 || !strings.Contains(stdout, "cleared") {
		t.Fatalf("exit %d: %q", code, stdout)
	}
	st, _ := LoadState(state)
	if st.Units["A"].Link != "" {
		t.Fatalf("link %q, want empty", st.Units["A"].Link)
	}
}

func TestViewMarksAndShowsALink(t *testing.T) {
	plan := writePlan(t, statusPlan)
	state := filepath.Join(t.TempDir(), "state.json")
	if code, _, stderr := runCLI("link", "A", "https://github.com/o/r/pull/42", "--plan", plan, "--state", state); code != 0 {
		t.Fatalf("link: %s", stderr)
	}

	m := newTestView(t, plan, state)
	m.cursor = 0
	out := m.View()
	if !strings.Contains(out, "github.com/o/r/pull/42") {
		t.Fatalf("the card should show the link:\n%s", out)
	}
	if !strings.Contains(out, "↗") {
		t.Fatalf("the row should be marked:\n%s", out)
	}

	// The detail pane shows it under LINK as well.
	m = press(t, m, "d")
	if detail := m.View(); !strings.Contains(detail, "LINK") {
		t.Fatalf("the detail pane should name the link:\n%s", detail)
	}
}

func TestOpenRefusesANonHttpLink(t *testing.T) {
	if err := openURL("file:///etc/passwd"); err == nil {
		t.Fatal("openURL must refuse a file url")
	}
}
