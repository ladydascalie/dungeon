package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// PlanHash is the sha256 of the plan file as dispatched. Any edit changes it,
// including a comment, because any edit changes what the lanes were told.
func PlanHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return hashPlan(data), nil
}

func hashPlan(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Plan is one encounter: who exists, and what each unit of work is.
type Plan struct {
	Encounter string `yaml:"encounter"`
	Repo      string `yaml:"repo"`
	// Project is the rail group the encounter workspace joins. Empty means the
	// repo folder name.
	Project     string   `yaml:"project"`
	Base        string   `yaml:"base"`
	Concurrency int      `yaml:"concurrency"`
	Split       string   `yaml:"split"` // right (default) or down/left/up: how a role's panes grow
	Minions     []Minion `yaml:"minions"`
	Units       []Unit   `yaml:"units"`
	Loops       []Loop   `yaml:"loops"`
}

type Minion struct {
	ID       string `yaml:"id"`
	Role     string `yaml:"role"`
	Worktree string `yaml:"worktree"`
	Of       string `yaml:"of"`
	Pane     string `yaml:"pane"`  // optional: run this minion in a pane the operator already has
	Tools    string `yaml:"tools"` // optional: comma separated allowlist that replaces the role default
}

type Unit struct {
	ID          string   `yaml:"id"`
	Minion      string   `yaml:"minion"`
	Paths       []string `yaml:"paths"`
	Deps        []Dep    `yaml:"deps"`
	Goal        string   `yaml:"goal"`
	Accept      string   `yaml:"accept"`
	Verdict     string   `yaml:"verdict"` // optional: the report line that proves this reviewer unit
	Provisional bool     `yaml:"-"`       // generated only: an unproven loop fix whose dep still counts as satisfied
	Loop        string   `yaml:"-"`       // generated only: the loop this unit belongs to
	Round       int      `yaml:"-"`       // generated only: 1-based round inside that loop
	Kind        string   `yaml:"-"`       // generated only: review or fix
	Split       *Split   `yaml:"split"`
}

// Dep is one unit this unit waits on. A bare scalar names a unit in the same
// plan. A mapping names a unit in another plan, so a repo can wait on work that
// landed somewhere else.
type Dep struct {
	Plan string `yaml:"plan,omitempty"`
	Unit string `yaml:"unit"`
}

// UnmarshalYAML reads either form. The mapping keys are read by hand because
// Node.Decode bypasses KnownFields: a misspelled key must fail the plan, not
// vanish.
func (d *Dep) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(n.Value) == "" {
			return fmt.Errorf("dep at line %d names no unit", n.Line)
		}
		d.Plan, d.Unit = "", n.Value
		return nil
	case yaml.MappingNode:
		var plan, unit string
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			// yaml.v3 refuses a duplicate key everywhere else in the plan, and
			// reading the keys by hand must not be a way around that.
			if seen[key.Value] {
				return fmt.Errorf("dep at line %d: key %q is already defined", key.Line, key.Value)
			}
			seen[key.Value] = true
			// A list or a mapping here would otherwise vanish into an empty
			// string, and the dep would quietly become a local one.
			if value.Kind != yaml.ScalarNode {
				return fmt.Errorf("dep at line %d: %s must be a plain value", key.Line, key.Value)
			}
			switch key.Value {
			case "plan":
				plan = value.Value
			case "unit":
				unit = value.Value
			default:
				return fmt.Errorf("dep at line %d has unknown key %q: a dep is a unit id or {plan, unit}", key.Line, key.Value)
			}
		}
		if strings.TrimSpace(unit) == "" {
			return fmt.Errorf("dep at line %d names no unit", n.Line)
		}
		// A mapping is the cross-plan form, so it must name a plan. A dep with
		// no plan is written as a bare unit id, and saying so beats a silent
		// local dep.
		if strings.TrimSpace(plan) == "" {
			return fmt.Errorf("dep at line %d names no plan: write %q as a bare unit id for a local dep, or {plan: <path>, unit: %s}", n.Line, unit, unit)
		}
		d.Plan, d.Unit = plan, unit
		return nil
	}
	return fmt.Errorf("dep at line %d is neither a unit id nor {plan, unit}", n.Line)
}

// MarshalYAML writes a local dep back as the bare scalar it was written as, so
// a plan with only local deps round trips unchanged.
func (d Dep) MarshalYAML() (any, error) {
	if d.Local() {
		return d.Unit, nil
	}
	return struct {
		Plan string `yaml:"plan"`
		Unit string `yaml:"unit"`
	}{d.Plan, d.Unit}, nil
}

// Local reports whether the dep names a unit in the same plan.
func (d Dep) Local() bool { return d.Plan == "" }

// String is the dep as one token: a unit id locally, plan#unit across plans.
func (d Dep) String() string {
	if d.Local() {
		return d.Unit
	}
	return d.Plan + "#" + d.Unit
}

// Loop is a review and fix cycle between two minions. The scheduler expands it
// into units; validation only checks that it could be expanded.
type Loop struct {
	ID         string   `yaml:"id"`
	Reviewer   string   `yaml:"reviewer"`
	Fixer      string   `yaml:"fixer"`
	Rounds     int      `yaml:"rounds"`
	Focus      string   `yaml:"focus"`
	Pass       string   `yaml:"pass"`
	Paths      []string `yaml:"paths"`
	Deps       []string `yaml:"deps"` // unit ids the first review waits on
	ReviewGoal string   `yaml:"review_goal"`
	FixGoal    string   `yaml:"fix_goal"`
}

type Split struct {
	Role  string `yaml:"role"`
	Count int    `yaml:"count"`
}

// Roles that own a worktree and may write. Everything else is staff work.
const (
	RoleScout      = "scout"
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
	RoleIntegrator = "integrator"
)

var roles = map[string]bool{
	RoleScout:      true,
	RoleWorker:     true,
	RoleReviewer:   true,
	RoleIntegrator: true,
}

func writes(role string) bool {
	return role == RoleWorker || role == RoleIntegrator
}

// Decode strictly rejects unknown fields: a misspelled key must fail the plan,
// not vanish.
func Decode(data []byte) (Plan, []Finding) {
	var p Plan
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return p, []Finding{{Code: EParse, Path: "", Message: "plan is empty"}}
		}
		return p, []Finding{{Code: EParse, Path: "", Message: err.Error()}}
	}
	return p, nil
}

func DecodeFile(path string) (Plan, []Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, nil, err
	}
	p, f := Decode(data)
	return p, f, nil
}
