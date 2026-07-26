// Package discover finds jsingo modules in a source tree.
//
// A directory is a module when it holds a package.json carrying a top-level
// "jsingo" key. Reusing package.json rather than inventing a marker file means
// editors, Dependabot, audit tooling and every other JavaScript tool already
// understand these directories, and there is no second manifest to drift out
// of sync with the first.
package discover

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Manifest is the "jsingo" section of a package.json.
type Manifest struct {
	// Entry is the entrypoint filename, relative to the module directory.
	Entry string `json:"entry"`
	// Out is the bundle filename. Empty means <entry basename>.bundle.js.
	Out string `json:"out"`
	// Target selects the bundler target. Empty means "node", which runs on
	// both node and bun; "bun" output does not run on node.
	Target string `json:"target"`
	// External lists imports to leave unbundled.
	External []string `json:"external"`
}

// Module is a discovered module directory.
type Module struct {
	// Dir is the absolute path of the directory holding package.json.
	Dir string
	// Name is the package.json "name", or the directory name.
	Name string
	// Manifest is the parsed jsingo section.
	Manifest Manifest
	// HasLockfile reports whether a committed lockfile is present.
	HasLockfile bool
	// HasNodeModules reports whether dependencies are installed in place.
	HasNodeModules bool
}

// EntryPath is the absolute path of the entrypoint.
func (m Module) EntryPath() string { return filepath.Join(m.Dir, m.Manifest.Entry) }

// OutPath is the absolute path of the bundle to produce.
func (m Module) OutPath() string {
	out := m.Manifest.Out
	if out == "" {
		base := filepath.Base(m.Manifest.Entry)
		out = strings.TrimSuffix(base, filepath.Ext(base)) + ".bundle.js"
	}
	return filepath.Join(m.Dir, out)
}

// Target returns the bundler target, defaulting to node.
//
// node output runs under bun as well; the reverse is not true, so node is the
// portable default.
func (m Module) Target() string {
	if m.Manifest.Target != "" {
		return m.Manifest.Target
	}
	return "node"
}

// packageJSON is the subset of package.json that matters here.
type packageJSON struct {
	Name         string          `json:"name"`
	Dependencies map[string]any  `json:"dependencies"`
	Jsingo       json.RawMessage `json:"jsingo"`
}

// skipDirs are never descended into.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".cache":       true,
}

// Find walks root and returns every jsingo module, sorted by directory.
func Find(root string) ([]Module, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	var mods []Module
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree should not abort discovery of the rest.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() != "package.json" {
			return nil
		}

		mod, ok, err := parse(filepath.Dir(path))
		if err != nil {
			return err
		}
		if ok {
			mods = append(mods, mod)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(mods, func(i, j int) bool { return mods[i].Dir < mods[j].Dir })
	return mods, nil
}

// parse reads a directory's package.json and reports whether it is a module.
func parse(dir string) (Module, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return Module{}, false, nil
	}

	var pkg packageJSON
	if err := json.Unmarshal(b, &pkg); err != nil {
		// A malformed package.json in an unrelated directory must not fail the
		// whole walk; only report it if it claims to be a jsingo module, which
		// we cannot know here.
		return Module{}, false, nil
	}
	if len(pkg.Jsingo) == 0 {
		return Module{}, false, nil
	}

	var man Manifest
	if err := json.Unmarshal(pkg.Jsingo, &man); err != nil {
		return Module{}, false, fmt.Errorf("%s: malformed \"jsingo\" section: %w",
			filepath.Join(dir, "package.json"), err)
	}
	if man.Entry == "" {
		return Module{}, false, fmt.Errorf("%s: \"jsingo\" section has no \"entry\"",
			filepath.Join(dir, "package.json"))
	}

	name := pkg.Name
	if name == "" {
		name = filepath.Base(dir)
	}

	m := Module{Dir: dir, Name: name, Manifest: man}
	for _, lock := range []string{"bun.lock", "bun.lockb", "package-lock.json", "pnpm-lock.yaml", "yarn.lock"} {
		if _, err := os.Stat(filepath.Join(dir, lock)); err == nil {
			m.HasLockfile = true
			break
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil && fi.IsDir() {
		m.HasNodeModules = true
	}
	// A module with no dependencies needs no lockfile, so do not demand one.
	if len(pkg.Dependencies) == 0 {
		m.HasLockfile = true
	}
	return m, true, nil
}
