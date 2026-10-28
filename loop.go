package main

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// A loop is a review and fix cycle. The plan declares it once; ExpandLoops
// turns it into ordinary units, so every command sees the same unit list and
// the scheduler needs no special case for rounds.
//
// The shape: rounds N produce N fixes and N+1 reviews. Review 1 triages, then
// each fix is judged by the review after it, so the last fix is judged too.
//
// A review unit is proven by its verdict line: the pass line is done, anything
// else is failed. A failed review advances the loop, because a failed dep
// satisfies the next unit. A done review stops the loop: StopLoopAfter cancels
// that round's fix and every later round.
//
// A fix unit is provisional and carries no accept command. It settles unproven,
// which proves only that the fixer stopped, and a provisional unproven dep
// releases the next review. The review is the judge, not a shell command.
func ExpandLoops(p Plan) (Plan, []Finding) {
	var f []Finding
	byID := map[string]Unit{}
	for _, u := range p.Units {
		byID[u.ID] = u
	}
	units := append([]Unit(nil), p.Units...)

	add := func(li int, want Unit) {
		if have, ok := byID[want.ID]; ok {
			// Idempotent: the same loop expanded twice must not double a unit.
			if !reflect.DeepEqual(have, want) {
				f = append(f, Finding{
					EDuplicateID, fmt.Sprintf("loops[%d].id", li),
					fmt.Sprintf("unit %q already exists and is not this loop's unit", want.ID),
				})
			}
			return
		}
		units = append(units, want)
		byID[want.ID] = want
	}

	for li, l := range p.Loops {
		for round := 1; round <= l.Rounds; round++ {
			add(li, loopReview(l, round))
			add(li, loopFix(l, round))
		}
		// No writer's output goes unjudged: the last fix gets a review too.
		add(li, loopReview(l, l.Rounds+1))
	}
	p.Units = units
	return p, f
}

// loopReview is the unit where the reviewer judges one round.
func loopReview(l Loop, round int) Unit {
	goal := strings.TrimSpace(l.ReviewGoal)
	if goal == "" {
		goal = fmt.Sprintf("Review round %d of loop %s. Focus: %s", round, l.ID, strings.TrimSpace(l.Focus))
	}
	u := Unit{
		ID:      fmt.Sprintf("%s-review-%d", l.ID, round),
		Minion:  l.Reviewer,
		Goal:    goal,
		Verdict: l.Pass,
	}
	if round > 1 {
		u.Deps = []string{fmt.Sprintf("%s-fix-%d", l.ID, round-1)}
	}
	return u
}

// loopFix is the unit where the fixer applies one review's findings. Its goal
// names the review report, which the brief also lists by absolute path.
func loopFix(l Loop, round int) Unit {
	reviewID := fmt.Sprintf("%s-review-%d", l.ID, round)
	goal := strings.TrimSpace(l.FixGoal)
	if goal == "" {
		goal = fmt.Sprintf("Fix what review %s found. Read its report: it is listed under \"reports from deps\".", reviewID)
	}
	return Unit{
		ID:          fmt.Sprintf("%s-fix-%d", l.ID, round),
		Minion:      l.Fixer,
		Paths:       append([]string(nil), l.Paths...),
		Deps:        []string{reviewID},
		Goal:        goal,
		Provisional: true,
	}
}

// PromoteLoopFix marks the fix a passing review just judged as done. A
// provisional fix settles unproven, and land merges only done units, so without
// this promotion the fixer's branch would never land. Review N judges fix N-1,
// so that is the fix it promotes, and only while that fix is unproven. A FAIL
// promotes nothing.
func PromoteLoopFix(p Plan, st *State, unitID string) {
	loopID, round, kind, ok := parseLoopUnit(p, unitID)
	if !ok || kind != "review" || round < 2 {
		return
	}
	fixID := fmt.Sprintf("%s-fix-%d", loopID, round-1)
	u, ok := findUnit(p, fixID)
	if !ok || !u.Provisional {
		return
	}
	if st.UnitStatus(fixID) != "unproven" {
		return
	}
	st.SetUnit(fixID, "done", "proved by "+unitID)
}

// StopLoopAfter cancels the planned units of the same loop that a passing
// review made pointless: its own round's fix, and every later round. It acts
// only on a review unit: a fixer finishing proves nothing about satisfaction.
func StopLoopAfter(p Plan, st *State, unitID string) {
	loopID, reviewRound, kind, ok := parseLoopUnit(p, unitID)
	if !ok || kind != "review" {
		return
	}
	note := fmt.Sprintf("loop %s satisfied at round %d", loopID, reviewRound)
	for _, u := range p.Units {
		loop, round, kind, ok := parseLoopUnit(p, u.ID)
		if !ok || loop != loopID {
			continue
		}
		if round < reviewRound || (round == reviewRound && kind != "fix") {
			continue
		}
		if st.UnitStatus(u.ID) == "planned" {
			st.SetUnit(u.ID, "canceled", note)
		}
	}
}

// parseLoopUnit splits a generated unit id back into its loop and round. The
// declared units and the loop list are the authority, so a unit named like a
// loop but not expanded from one never matches.
func parseLoopUnit(p Plan, unitID string) (string, int, string, bool) {
	for _, l := range p.Loops {
		for _, kind := range []string{"review", "fix"} {
			prefix := l.ID + "-" + kind + "-"
			if !strings.HasPrefix(unitID, prefix) {
				continue
			}
			round, err := strconv.Atoi(strings.TrimPrefix(unitID, prefix))
			limit := l.Rounds
			if kind == "review" {
				limit++ // the review that judges the last fix
			}
			if err == nil && round >= 1 && round <= limit {
				return l.ID, round, kind, true
			}
		}
	}
	return "", 0, "", false
}

// loadPlan decodes, validates and expands a plan. Every command goes through it
// so a loop is one unit list everywhere: check, status, view, up, next, land.
func loadPlan(path string) (Plan, []Finding, error) {
	p, findings, err := DecodeFile(path)
	if err != nil || len(findings) > 0 {
		return p, findings, err
	}
	if findings = Validate(p); len(findings) > 0 {
		return p, findings, nil
	}
	p, findings = ExpandLoops(p)
	return p, findings, nil
}
