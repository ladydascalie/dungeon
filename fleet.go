package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// FleetModel is the whole fleet as plain data: one row per unit over every
// encounter discoverPlans finds. It renders nothing, starts nothing, and keeps
// no state of its own.
type FleetModel struct {
	Encounters []FleetEncounter `json:"encounters"`
	Rows       []FleetRow       `json:"rows"`
	Sources    FleetSources     `json:"sources"`
}

// FleetEncounter is one encounter the fleet looked at. Err carries a plan or
// state that could not be read, so the view can name it instead of dropping it.
type FleetEncounter struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	LastTouch time.Time `json:"last_touch"`
	Err       string    `json:"err,omitempty"`
}

// FleetRow is one unit. Status, pane, worktree, branch, ahead and dirty come
// from the state file alone. WT adds facts worktrunk can prove, and Review says
// whether a hunk session is open on this worktree.
type FleetRow struct {
	Encounter  string       `json:"encounter"`
	ID         string       `json:"id"`
	Minion     string       `json:"minion"`
	Role       string       `json:"role"`
	Status     string       `json:"status"`
	Pane       string       `json:"pane,omitempty"`
	Worktree   string       `json:"worktree,omitempty"`
	Branch     string       `json:"branch,omitempty"`
	Ahead      int          `json:"ahead"`
	Dirty      int          `json:"dirty"`
	Accept     string       `json:"accept,omitempty"`
	LastReport *time.Time   `json:"last_report,omitempty"`
	LastTouch  time.Time    `json:"last_touch"`
	WT         *FleetWT     `json:"wt,omitempty"`
	Review     *FleetReview `json:"review,omitempty"`
}

// FleetWT is what worktrunk says about one worktree. A nil pointer on a row
// means worktrunk had no fact for that path, not that the numbers are zero.
type FleetWT struct {
	Added   *int         `json:"added,omitempty"`
	Deleted *int         `json:"deleted,omitempty"`
	PR      *FleetPR     `json:"pr,omitempty"`
	Checks  *FleetChecks `json:"checks,omitempty"`
}

type FleetPR struct {
	Number    int    `json:"number,omitempty"`
	URL       string `json:"url,omitempty"`
	Review    string `json:"review,omitempty"`
	Mergeable *bool  `json:"mergeable,omitempty"`
}

type FleetChecks struct {
	Status string `json:"status,omitempty"`
	Source string `json:"source,omitempty"`
	Stale  bool   `json:"stale,omitempty"`
}

// FleetReview is a live hunk review on a worktree.
type FleetReview struct {
	Session string `json:"session"`
}

type FleetSources struct {
	Worktrunk FleetSource `json:"worktrunk"`
	Hunk      FleetSource `json:"hunk"`
}

// FleetSource says whether a source answered. A source that failed contributes
// nothing and is marked unavailable; the model still renders.
type FleetSource struct {
	Available bool   `json:"available"`
	Err       string `json:"err,omitempty"`
}

// CommandRunner is the only way the model reaches a subprocess. Tests pass a
// fake runner, so a missing tool is an unavailable source, never a failure.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

type FleetOptions struct {
	Run   CommandRunner
	Cache *FleetCache
	Now   func() time.Time
}

// The worktrunk call is the expensive one. A few seconds keeps a refresh tick
// from repeating it without showing a figure that has already gone stale.
const fleetWTCacheTTL = 5 * time.Second

// FleetCache holds the last worktrunk answer per repo. The zero value is ready.
// It is passed in rather than kept in a global, so the caller owns its lifetime.
type FleetCache struct {
	mu sync.Mutex
	wt map[string]fleetWTCacheEntry
}

type fleetWTCacheEntry struct {
	At        time.Time
	Facts     map[string]FleetWT
	Available bool
	Err       string
}

func (c *FleetCache) lookup(repo string, now time.Time) (fleetWTCacheEntry, bool) {
	if c == nil {
		return fleetWTCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.wt[repo]
	if !ok || now.Sub(entry.At) >= fleetWTCacheTTL {
		return fleetWTCacheEntry{}, false
	}
	return entry, true
}

func (c *FleetCache) store(repo string, entry fleetWTCacheEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wt == nil {
		c.wt = map[string]fleetWTCacheEntry{}
	}
	c.wt[repo] = entry
}

// BuildFleetModel reads every encounter discoverPlans finds and returns one
// snapshot. An unreadable encounter is named in Encounters and contributes no
// rows; the rest still load.
func BuildFleetModel(ctx context.Context, opts FleetOptions) FleetModel {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	run := opts.Run
	if run == nil {
		run = execCommand
	}

	model := FleetModel{
		Encounters: []FleetEncounter{},
		Rows:       []FleetRow{},
	}
	type loaded struct {
		name  string
		path  string
		plan  Plan
		state State
		touch time.Time
	}
	var encounters []loaded
	repos := map[string]bool{}

	for _, e := range discoverPlans() {
		row := FleetEncounter{
			Name:      e.Name,
			Path:      e.Path,
			LastTouch: lastTouched(e.Path),
		}
		if e.Err != nil {
			row.Err = e.Err.Error()
			model.Encounters = append(model.Encounters, row)
			continue
		}
		plan, findings, err := DecodeFile(e.Path)
		if err != nil {
			row.Err = err.Error()
			model.Encounters = append(model.Encounters, row)
			continue
		}
		if len(findings) > 0 {
			row.Err = findings[0].String()
			model.Encounters = append(model.Encounters, row)
			continue
		}
		state, err := LoadState(StatePath(e.Path))
		if err != nil {
			row.Err = err.Error()
			model.Encounters = append(model.Encounters, row)
			continue
		}
		model.Encounters = append(model.Encounters, row)
		name := plan.Encounter
		if name == "" {
			name = e.Name
		}
		encounters = append(encounters, loaded{name: name, path: e.Path, plan: plan, state: state, touch: row.LastTouch})
		if plan.Repo != "" {
			repos[plan.Repo] = true
		}
	}

	repoList := make([]string, 0, len(repos))
	for repo := range repos {
		repoList = append(repoList, repo)
	}
	sort.Strings(repoList)

	facts, wtSource := loadFleetWT(ctx, run, opts.Cache, now(), repoList)
	model.Sources.Worktrunk = wtSource
	reviews := map[string]string{}
	hunkSource := FleetSource{Err: "no encounter to join"}
	if len(encounters) > 0 {
		reviews, hunkSource = loadFleetReviews(ctx, run)
	}
	model.Sources.Hunk = hunkSource

	for _, enc := range encounters {
		roleOf := map[string]string{}
		for _, m := range enc.plan.Minions {
			roleOf[m.ID] = m.Role
		}
		for _, u := range enc.plan.Units {
			ms := enc.state.Minions[u.Minion]
			row := FleetRow{
				Encounter: enc.name,
				ID:        u.ID,
				Minion:    u.Minion,
				Role:      roleOf[u.Minion],
				Status:    enc.state.UnitStatus(u.ID),
				Pane:      ms.Pane,
				Worktree:  ms.Worktree,
				Branch:    ms.Branch,
				Ahead:     ms.Ahead,
				Dirty:     ms.Dirty,
				Accept:    u.Accept,
				LastTouch: enc.touch,
			}
			if row.Worktree != "" {
				if fact, ok := facts[fleetPath(row.Worktree)]; ok {
					row.WT = copyFleetWT(fact)
				}
				if session, ok := reviews[fleetPath(row.Worktree)]; ok {
					row.Review = &FleetReview{Session: session}
				}
			}
			report := filepath.Join(filepath.Dir(enc.path), "reports", u.ID+".md")
			if info, err := os.Stat(report); err == nil {
				at := info.ModTime()
				row.LastReport = &at
			}
			model.Rows = append(model.Rows, row)
		}
	}
	return model
}

func fleetPath(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func copyFleetWT(in FleetWT) *FleetWT {
	out := in
	if in.Added != nil {
		v := *in.Added
		out.Added = &v
	}
	if in.Deleted != nil {
		v := *in.Deleted
		out.Deleted = &v
	}
	if in.PR != nil {
		pr := *in.PR
		out.PR = &pr
	}
	if in.Checks != nil {
		checks := *in.Checks
		out.Checks = &checks
	}
	return &out
}

type fleetWTEnvelope struct {
	Schema int           `json:"schema"`
	Items  []fleetWTItem `json:"items"`
}

type fleetWTItem struct {
	Worktree *struct {
		Path    string `json:"path"`
		Changes *struct {
			Diff *struct {
				Added   int `json:"added"`
				Deleted int `json:"deleted"`
			} `json:"diff"`
		} `json:"changes"`
	} `json:"worktree"`
	PR *struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Review    string `json:"review"`
		Mergeable *bool  `json:"mergeable"`
	} `json:"pr"`
	Checks *struct {
		Status string `json:"status"`
		Source string `json:"source"`
		Stale  bool   `json:"stale"`
	} `json:"checks"`
}

// loadFleetWT asks worktrunk once per repo. The whole source is unavailable if
// any repo failed: a partial answer must not look like a complete one.
func loadFleetWT(ctx context.Context, run CommandRunner, cache *FleetCache, now time.Time, repos []string) (map[string]FleetWT, FleetSource) {
	if len(repos) == 0 {
		return nil, FleetSource{Err: "no encounter repo to read"}
	}
	all := map[string]FleetWT{}
	for _, repo := range repos {
		entry, ok := cache.lookup(repo, now)
		if !ok {
			out, err := run(ctx, "wt", "-C", repo, "list", "--format=json", "--full")
			if err != nil {
				entry = fleetWTCacheEntry{At: now, Err: fmt.Sprintf("wt: %v", err)}
			} else if facts, err := parseFleetWT(out); err != nil {
				entry = fleetWTCacheEntry{At: now, Err: fmt.Sprintf("wt: %v", err)}
			} else {
				entry = fleetWTCacheEntry{At: now, Facts: facts, Available: true}
			}
			cache.store(repo, entry)
		}
		if !entry.Available {
			return nil, FleetSource{Err: entry.Err}
		}
		for path, fact := range entry.Facts {
			all[path] = fact
		}
	}
	return all, FleetSource{Available: true}
}

func parseFleetWT(out []byte) (map[string]FleetWT, error) {
	var env fleetWTEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	if env.Schema != 2 {
		return nil, fmt.Errorf("unsupported wt JSON schema %d", env.Schema)
	}
	facts := map[string]FleetWT{}
	for _, item := range env.Items {
		if item.Worktree == nil || item.Worktree.Path == "" {
			continue
		}
		fact := FleetWT{}
		if item.Worktree.Changes != nil && item.Worktree.Changes.Diff != nil {
			added, deleted := item.Worktree.Changes.Diff.Added, item.Worktree.Changes.Diff.Deleted
			fact.Added, fact.Deleted = &added, &deleted
		}
		if item.PR != nil {
			fact.PR = &FleetPR{
				Number:    item.PR.Number,
				URL:       item.PR.URL,
				Review:    item.PR.Review,
				Mergeable: item.PR.Mergeable,
			}
		}
		if item.Checks != nil {
			fact.Checks = &FleetChecks{
				Status: item.Checks.Status,
				Source: item.Checks.Source,
				Stale:  item.Checks.Stale,
			}
		}
		facts[fleetPath(item.Worktree.Path)] = fact
	}
	return facts, nil
}

type fleetHunkEnvelope struct {
	Sessions []struct {
		SessionID string `json:"sessionId"`
		RepoRoot  string `json:"repoRoot"`
	} `json:"sessions"`
}

// loadFleetReviews joins live hunk sessions by repo root. Hunk reports its
// session's current repo root, which is the worktree the review is open on.
func loadFleetReviews(ctx context.Context, run CommandRunner) (map[string]string, FleetSource) {
	out, err := run(ctx, "hunk", "session", "list", "--json")
	if err != nil {
		return nil, FleetSource{Err: fmt.Sprintf("hunk: %v", err)}
	}
	var env fleetHunkEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, FleetSource{Err: fmt.Sprintf("hunk: %v", err)}
	}
	reviews := map[string]string{}
	for _, session := range env.Sessions {
		if session.SessionID == "" || session.RepoRoot == "" {
			continue
		}
		reviews[fleetPath(session.RepoRoot)] = session.SessionID
	}
	return reviews, FleetSource{Available: true}
}
