// Package detect locates a usable JavaScript runtime.
//
// Preference order is bun, then node, then deno. Bun leads on idle memory and
// cold start; node is a first-class fallback rather than a degraded mode,
// because the jsingo protocol needs only a stream socket and both provide one.
package detect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind identifies a JavaScript runtime.
type Kind string

const (
	KindBun  Kind = "bun"
	KindNode Kind = "node"
	KindDeno Kind = "deno"
)

// DefaultOrder is the preference order used when none is given.
var DefaultOrder = []Kind{KindBun, KindNode, KindDeno}

// minVersion is the oldest release of each runtime we accept.
//
// Node 18 is the floor because the host relies on stable AbortSignal and
// global fetch. Bun 1.0 is its first stable release. Deno 1.30 aligns on
// Node-compat APIs the host uses.
var minVersion = map[Kind]Version{
	KindBun:  {1, 0, 0},
	KindNode: {18, 0, 0},
	KindDeno: {1, 30, 0},
}

// Errors returned by Find.
var (
	// ErrNotFound means no candidate runtime was present on the system.
	ErrNotFound = errors.New("detect: no JavaScript runtime found")
	// ErrTooOld means a runtime was found but predates the minimum version.
	ErrTooOld = errors.New("detect: JavaScript runtime too old")
)

// Version is a parsed semantic version. Pre-release and build metadata are
// discarded: they never affect the compatibility decision.
type Version struct{ Major, Minor, Patch int }

// String implements fmt.Stringer.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v precedes o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Runtime is a located, version-checked JavaScript runtime.
type Runtime struct {
	Kind    Kind
	Path    string
	Version Version
}

// String implements fmt.Stringer.
func (r Runtime) String() string {
	return fmt.Sprintf("%s %s (%s)", r.Kind, r.Version, r.Path)
}

// Command builds the argv for running entry under this runtime, applying the
// flags appropriate to each.
//
// The flags are deliberately conservative rather than clever. Bun's --smol
// trades a little throughput for a materially smaller heap, which suits a
// sidecar. Node's --disallow-code-generation-from-strings removes eval and
// new Function from reach of any npm dependency in the bundle. Deno's
// permissions are narrowed to what a bundled sidecar can legitimately need.
func (r Runtime) Command(ctx context.Context, entry string, extra ...string) *exec.Cmd {
	var args []string
	switch r.Kind {
	case KindBun:
		args = append(args, "--smol", entry)
	case KindNode:
		args = append(args, "--disallow-code-generation-from-strings", entry)
	case KindDeno:
		args = append(args, "run", "--allow-read", "--allow-env", entry)
	default:
		args = append(args, entry)
	}
	args = append(args, extra...)
	return exec.CommandContext(ctx, r.Path, args...)
}

// Option configures Find.
type Option func(*config)

type config struct {
	order    []Kind
	lookup   func(string) (string, error)
	extra    []string
	extraSet bool
	timeout  time.Duration
}

// WithOrder overrides the preference order. An empty order is ignored.
func WithOrder(order ...Kind) Option {
	return func(c *config) {
		if len(order) > 0 {
			c.order = order
		}
	}
}

// WithLookPath overrides PATH resolution. Intended for tests.
func WithLookPath(f func(string) (string, error)) Option {
	return func(c *config) {
		if f != nil {
			c.lookup = f
		}
	}
}

// WithSearchDirs sets the directories searched when PATH resolution fails,
// replacing the defaults returned by [DefaultSearchDirs].
//
// It replaces rather than appends so a caller can opt out of the defaults
// entirely: WithSearchDirs() with no arguments restricts Find to PATH, which
// is what hermetic tests and locked-down deployments need. To keep the
// defaults and add to them, pass append(detect.DefaultSearchDirs(), extra...).
func WithSearchDirs(dirs ...string) Option {
	return func(c *config) {
		c.extra = dirs
		c.extraSet = true
	}
}

// WithTimeout bounds each `--version` probe. Zero selects the default.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// defaultProbeTimeout bounds a single `--version` invocation. Generous enough
// for a cold binary on a loaded machine, short enough that a wedged one does
// not stall startup.
const defaultProbeTimeout = 5 * time.Second

// Find returns the first runtime in preference order that is present and new
// enough.
//
// If every candidate is missing it returns ErrNotFound. If candidates were
// found but all were too old it returns ErrTooOld, naming the newest one seen
// so the message is actionable rather than just a refusal.
func Find(ctx context.Context, opts ...Option) (Runtime, error) {
	cfg := config{
		order:   DefaultOrder,
		lookup:  exec.LookPath,
		timeout: defaultProbeTimeout,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if !cfg.extraSet {
		cfg.extra = DefaultSearchDirs()
	}

	var tooOld []Runtime
	for _, kind := range cfg.order {
		path, err := resolve(string(kind), cfg)
		if err != nil {
			continue
		}
		v, err := probeVersion(ctx, path, cfg.timeout)
		if err != nil {
			// A binary that will not report a version cannot be trusted to run
			// the host; treat it as absent and keep looking.
			continue
		}
		rt := Runtime{Kind: kind, Path: path, Version: v}
		if v.Less(minVersion[kind]) {
			tooOld = append(tooOld, rt)
			continue
		}
		return rt, nil
	}

	if len(tooOld) > 0 {
		best := tooOld[0]
		return Runtime{}, fmt.Errorf("%w: found %s, need %s or newer",
			ErrTooOld, best, minVersion[best.Kind])
	}
	return Runtime{}, fmt.Errorf("%w: tried %s", ErrNotFound, kindList(cfg.order))
}

// resolve finds a binary on PATH, falling back to well-known install
// directories that a non-login shell often omits.
func resolve(name string, cfg config) (string, error) {
	if p, err := cfg.lookup(name); err == nil {
		return p, nil
	}
	for _, dir := range cfg.extra {
		p := filepath.Join(dir, name)
		if isExecutableFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q not found", name)
}

// DefaultSearchDirs lists locations installers use that are frequently missing
// from a non-interactive PATH: launchd and systemd services routinely see a
// minimal PATH, which is a common cause of "works in my terminal" reports.
//
// It is exported so callers can extend the defaults rather than replace them:
//
//	detect.WithSearchDirs(append(detect.DefaultSearchDirs(), "/opt/js/bin")...)
func DefaultSearchDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".deno", "bin"),
			filepath.Join(home, ".volta", "bin"),
			filepath.Join(home, ".nvm", "current", "bin"),
			filepath.Join(home, ".local", "bin"),
		)
	}
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// versionRe matches the first dotted numeric triple in a version banner.
// Runtimes disagree on format - bun prints "1.1.20", node "v22.1.0", deno
// "deno 1.44.0 (release, aarch64-apple-darwin)" over several lines - so the
// pattern targets the one thing they share.
var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func probeVersion(ctx context.Context, path string, timeout time.Duration) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Only stdout is considered: a runtime that prints warnings to stderr
	// should not have them parsed as a version.
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return Version{}, fmt.Errorf("probe %s: %w", path, err)
	}
	return ParseVersion(string(out))
}

// ParseVersion extracts the first semantic version from a runtime's banner.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("no version in %q", truncate(s, 64))
	}
	// The regexp guarantees three digit runs; Atoi can only fail on overflow,
	// which a version string will not produce in practice.
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return Version{major, minor, patch}, nil
}

func kindList(kinds []Kind) string {
	s := make([]string, len(kinds))
	for i, k := range kinds {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
