package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// PlanHash is the sha256 of the plan file as dispatched. Any edit changes it,
// including a comment, because any edit changes what the lanes were told.
func PlanHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Plan is one encounter: who exists, and what each unit of work is.
type Plan struct {
	Encounter   string   `yaml:"encounter"`
	Repo        string   `yaml:"repo"`
	Base        string   `yaml:"base"`
	Concurrency int      `yaml:"concurrency"`
	Split       string   `yaml:"split"` // right (default) or down/left/up: how a role's panes grow
	Minions     []Minion `yaml:"minions"`
	Units       []Unit   `yaml:"units"`
}

type Minion struct {
	ID       string `yaml:"id"`
	Role     string `yaml:"role"`
	Worktree string `yaml:"worktree"`
	Of       string `yaml:"of"`
	Pane     string `yaml:"pane"` // optional: run this minion in a pane the operator already has
}

type Unit struct {
	ID     string   `yaml:"id"`
	Minion string   `yaml:"minion"`
	Paths  []string `yaml:"paths"`
	Deps   []string `yaml:"deps"`
	Goal   string   `yaml:"goal"`
	Accept string   `yaml:"accept"`
	Split  *Split   `yaml:"split"`
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
