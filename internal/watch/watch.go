// Package watch turns filesystem noise into precise "this function's code
// changed" signals, debounced so editor save-storms trigger one reload.
package watch

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/geetnsh2k1/pulse/internal/config"
)

const debounce = 300 * time.Millisecond

var ignoredDirs = map[string]bool{
	".pulse": true, "node_modules": true, "__pycache__": true, ".git": true,
	".idea": true, ".vscode": true, ".venv": true, "venv": true, "dist": true,
}

type Watcher struct {
	cfg      *config.Config
	onChange func(functions []string, reason string) // functions nil => pulse.yaml itself
	fsw      *fsnotify.Watcher
	done     chan struct{}

	codeDirs []fnDir
}

type fnDir struct {
	fn  string
	dir string // absolute
}

func New(cfg *config.Config, onChange func(functions []string, reason string)) *Watcher {
	return &Watcher{cfg: cfg, onChange: onChange, done: make(chan struct{})}
}

func (w *Watcher) Start() error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw

	// Project root: catches pulse.yaml edits.
	if err := fsw.Add(w.cfg.Root); err != nil {
		return err
	}
	for _, name := range w.cfg.FunctionNames() {
		dir := filepath.Join(w.cfg.Root, w.cfg.Functions[name].CodeDir)
		w.codeDirs = append(w.codeDirs, fnDir{fn: name, dir: dir})
		if err := w.addTree(dir); err != nil {
			return err
		}
	}

	go w.loop()
	return nil
}

func (w *Watcher) Stop() {
	close(w.done)
	if w.fsw != nil {
		_ = w.fsw.Close()
	}
}

// addTree registers dir and every non-ignored subdirectory.
func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // unreadable subtrees are skipped, not fatal
		}
		if ignoredDirs[d.Name()] && path != root {
			return filepath.SkipDir
		}
		_ = w.fsw.Add(path)
		return nil
	})
}

func (w *Watcher) loop() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}

	pendingFns := map[string]bool{}
	configChanged := false
	changeCount := 0

	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod || ignoredPath(ev.Name) {
				continue
			}
			if samePath(ev.Name, w.cfg.Path) {
				configChanged = true
				timer.Reset(debounce)
				continue
			}

			hit := false
			for _, fd := range w.codeDirs {
				if within(fd.dir, ev.Name) {
					pendingFns[fd.fn] = true
					hit = true
				}
			}
			if !hit {
				continue
			}
			changeCount++
			// New directories need their own watches (created nested trees).
			if ev.Op.Has(fsnotify.Create) {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() && !ignoredDirs[filepath.Base(ev.Name)] {
					_ = w.addTree(ev.Name)
				}
			}
			timer.Reset(debounce)

		case <-timer.C:
			if configChanged {
				w.onChange(nil, "pulse.yaml")
			}
			if len(pendingFns) > 0 {
				fns := make([]string, 0, len(pendingFns))
				for fn := range pendingFns {
					fns = append(fns, fn)
				}
				sort.Strings(fns)
				reason := "1 change"
				if changeCount > 1 {
					reason = strconv.Itoa(changeCount) + " changes"
				}
				w.onChange(fns, reason)
			}
			pendingFns = map[string]bool{}
			configChanged = false
			changeCount = 0

		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}

		case <-w.done:
			return
		}
	}
}

func ignoredPath(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") && base != "." {
		return true
	}
	if strings.HasSuffix(base, "~") || strings.HasSuffix(base, ".swp") ||
		strings.HasSuffix(base, ".swx") || strings.HasSuffix(base, ".tmp") {
		return true
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if ignoredDirs[seg] {
			return true
		}
	}
	return false
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	ra, err1 := filepath.Abs(a)
	rb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && ra == rb
}
