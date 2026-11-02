package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// The rail also wakes on the files it reads: a state write, a new report, a
// plan edit and an OSC record all land in one of a few directories. fsnotify
// turns each into a wake-up, so the operator sees a report as soon as a lane
// writes it instead of on the next tick.

type watchEvent struct {
	Path string
	Op   fsnotify.Op
	Err  error
	At   time.Time
}

type fsWatcher struct {
	ctx context.Context
	w   *fsnotify.Watcher
	out chan<- watchEvent
	osc map[string]bool
}

// railWatchDirs is the set the rail watches: the repo's .dungeon tree (every
// encounter, reports and briefs included) plus the OSC record directory for
// this server.
func railWatchDirs() (dirs, oscDirs []string) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil
	}
	if root, ok := railEncounterRoot(cwd); ok {
		dirs = watchTreeDirs(root)
	}
	osc := oscStateDir(oscServerKey())
	if err := os.MkdirAll(osc, 0o700); err == nil {
		oscDirs = []string{osc}
	}
	return dirs, oscDirs
}

// watchTreeDirs lists root and every directory under it. fsnotify watches
// directories, not files, so a nested encounter, reports/ and briefs/ each
// need their own watch. The tree is small and bounded; a runaway tree stops at
// one thousand directories rather than growing the watch list forever.
func watchTreeDirs(root string) []string {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		dirs = append(dirs, path)
		if len(dirs) >= 1000 {
			return filepath.SkipAll
		}
		return nil
	})
	return dirs
}

// railWatch watches the rail's own two sources: the repo's .dungeon tree and
// this server's OSC records.
func railWatch(ctx context.Context, out chan<- watchEvent) (*fsWatcher, error) {
	dirs, osc := railWatchDirs()
	return startWatch(ctx, out, dirs, osc)
}

// startWatch adds the directories and starts one reader goroutine. A watcher
// with no directories is valid: the rail still has tmux events and a backstop.
func startWatch(ctx context.Context, out chan<- watchEvent, dirs, oscDirs []string) (*fsWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fsWatcher{ctx: ctx, w: w, out: out, osc: map[string]bool{}}
	for _, dir := range oscDirs {
		if dir == "" {
			continue
		}
		fw.osc[filepath.Clean(dir)] = true
		if err := fw.add(dir); err != nil {
			_ = w.Close()
			return nil, err
		}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := fw.add(dir); err != nil {
			_ = w.Close()
			return nil, err
		}
	}
	go fw.run()
	return fw, nil
}

func (fw *fsWatcher) add(dir string) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	if err := fw.w.Add(dir); err != nil {
		if errors.Is(err, fsnotify.ErrClosed) {
			return nil
		}
		return err
	}
	return nil
}

func (fw *fsWatcher) want(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "plan.yaml", "state.json":
		return true
	}
	if strings.HasSuffix(base, ".tmp") {
		return false // SaveState writes through a temporary file
	}
	if strings.HasSuffix(base, ".json") && fw.osc[filepath.Clean(filepath.Dir(path))] {
		return true
	}
	dir := filepath.ToSlash(filepath.Dir(path))
	for _, part := range strings.Split(dir, "/") {
		if part == "reports" || part == "briefs" {
			return true
		}
	}
	return false
}

func (fw *fsWatcher) run() {
	defer fw.w.Close()
	for {
		select {
		case <-fw.ctx.Done():
			return
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = fw.add(ev.Name)
				}
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			if !fw.want(ev.Name) {
				continue
			}
			fw.emit(watchEvent{Path: ev.Name, Op: ev.Op})
		case err, ok := <-fw.w.Errors:
			if !ok {
				return
			}
			fw.emit(watchEvent{Err: err})
		}
	}
}

func (fw *fsWatcher) emit(ev watchEvent) {
	if fw.out == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case fw.out <- ev:
	default:
		// Coalesced events plus the backstop cover a dropped one.
	}
}
