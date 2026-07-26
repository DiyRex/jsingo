package detect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in string
		want     Version
		wantErr  bool
	}{
		{"bun", "1.1.20\n", Version{1, 1, 20}, false},
		{"node", "v22.1.0\n", Version{22, 1, 0}, false},
		{"deno banner", "deno 1.44.0 (release, aarch64-apple-darwin)\nv8 12.4\ntypescript 5.4", Version{1, 44, 0}, false},
		{"prerelease suffix", "1.2.3-canary.42\n", Version{1, 2, 3}, false},
		{"leading noise", "warning: something\n1.0.0", Version{1, 0, 0}, false},
		{"multi digit", "v120.11.99", Version{120, 11, 99}, false},
		{"no version", "command not found", Version{}, true},
		{"two components only", "v22.1", Version{}, true},
		{"empty", "", Version{}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVersionLess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b Version
		want bool
	}{
		{Version{1, 0, 0}, Version{2, 0, 0}, true},
		{Version{2, 0, 0}, Version{1, 9, 9}, false},
		{Version{1, 2, 0}, Version{1, 10, 0}, true}, // numeric, not lexical
		{Version{1, 2, 3}, Version{1, 2, 3}, false},
		{Version{1, 2, 4}, Version{1, 2, 3}, false},
		{Version{18, 0, 0}, Version{18, 0, 1}, true},
	}
	for _, tc := range tests {
		if got := tc.a.Less(tc.b); got != tc.want {
			t.Errorf("%v.Less(%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// fakeRuntime writes an executable script that prints banner for --version.
func fakeRuntime(t *testing.T, dir, name, banner string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake runtimes use shell scripts")
	}
	path := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%%s' '%s'; fi\n", banner)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// lookupIn resolves names against dir only, standing in for PATH.
func lookupIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if isExecutableFile(p) {
			return p, nil
		}
		return "", fmt.Errorf("%q not found", name)
	}
}

// noLookup simulates an empty PATH.
func noLookup(name string) (string, error) {
	return "", fmt.Errorf("%q not found", name)
}

func TestFindPrefersBun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "bun", "1.1.20")
	fakeRuntime(t, dir, "node", "v22.0.0")

	rt, err := Find(t.Context(), WithLookPath(lookupIn(dir)), WithSearchDirs())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindBun {
		t.Fatalf("got %v, want bun to win", rt.Kind)
	}
	if rt.Version != (Version{1, 1, 20}) {
		t.Fatalf("version = %v", rt.Version)
	}
}

func TestFindFallsBackToNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "node", "v22.1.0")

	rt, err := Find(t.Context(), WithLookPath(lookupIn(dir)), WithSearchDirs())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindNode {
		t.Fatalf("got %v, want node", rt.Kind)
	}
}

func TestFindRespectsExplicitOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "bun", "1.1.20")
	fakeRuntime(t, dir, "node", "v22.1.0")

	rt, err := Find(t.Context(),
		WithLookPath(lookupIn(dir)), WithSearchDirs(), WithOrder(KindNode, KindBun))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindNode {
		t.Fatalf("got %v, want node to win under an explicit order", rt.Kind)
	}
}

// An old runtime must not be silently skipped in favour of "not found": the
// operator needs to know an upgrade would fix it.
func TestFindReportsTooOld(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "node", "v16.20.0")

	_, err := Find(t.Context(), WithLookPath(lookupIn(dir)), WithSearchDirs())
	if !errors.Is(err, ErrTooOld) {
		t.Fatalf("got %v, want ErrTooOld", err)
	}
	if !strings.Contains(err.Error(), "16.20.0") || !strings.Contains(err.Error(), "18.0.0") {
		t.Fatalf("message should name both versions, got: %v", err)
	}
}

// A too-old runtime earlier in the order must not shadow a usable later one.
func TestFindSkipsOldInFavourOfNewer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "bun", "0.8.0") // below the 1.0 floor
	fakeRuntime(t, dir, "node", "v22.1.0")

	rt, err := Find(t.Context(), WithLookPath(lookupIn(dir)), WithSearchDirs())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindNode {
		t.Fatalf("got %v, want node after bun failed the floor", rt.Kind)
	}
}

func TestFindNotFound(t *testing.T) {
	t.Parallel()

	_, err := Find(t.Context(), WithLookPath(noLookup), WithSearchDirs())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	for _, k := range DefaultOrder {
		if !strings.Contains(err.Error(), string(k)) {
			t.Errorf("message should name %q: %v", k, err)
		}
	}
}

// Services often run with a minimal PATH, so installer directories must still
// be searched. This is the usual cause of "works in my terminal" failures.
func TestFindSearchesExtraDirsWhenPathFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "bun", "1.1.20")

	rt, err := Find(t.Context(), WithLookPath(noLookup), WithSearchDirs(dir))
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindBun || rt.Path != filepath.Join(dir, "bun") {
		t.Fatalf("got %+v, want the bun in the extra dir", rt)
	}
}

// A binary that exists but cannot report a version is unusable; Find should
// move on rather than fail outright.
func TestFindSkipsUnprobeableBinary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeRuntime(t, dir, "bun", "") // prints nothing for --version
	fakeRuntime(t, dir, "node", "v22.1.0")

	rt, err := Find(t.Context(), WithLookPath(lookupIn(dir)), WithSearchDirs())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if rt.Kind != KindNode {
		t.Fatalf("got %v, want node", rt.Kind)
	}
}

func TestFindIgnoresDirectoriesAndNonExecutables(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte("not executable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Find(t.Context(), WithLookPath(noLookup), WithSearchDirs(dir))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A wedged binary must not stall startup indefinitely.
func TestFindProbeTimeout(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bun")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := time.Now()
	_, err := Find(t.Context(),
		WithLookPath(lookupIn(dir)), WithSearchDirs(), WithTimeout(100*time.Millisecond))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe took %v; the timeout was not honoured", elapsed)
	}
}

func TestCommandFlagsPerRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind      Kind
		wantFlags []string
	}{
		{KindBun, []string{"--smol"}},
		{KindNode, []string{"--disallow-code-generation-from-strings"}},
		{KindDeno, []string{"run", "--allow-read", "--allow-env"}},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()

			rt := Runtime{Kind: tc.kind, Path: "/fake/" + string(tc.kind)}
			cmd := rt.Command(t.Context(), "host.js", "--extra")

			args := strings.Join(cmd.Args, " ")
			for _, f := range tc.wantFlags {
				if !strings.Contains(args, f) {
					t.Errorf("missing %q in %v", f, cmd.Args)
				}
			}
			// The entry must precede script arguments, or the runtime treats
			// them as its own.
			entryAt, extraAt := indexOf(cmd.Args, "host.js"), indexOf(cmd.Args, "--extra")
			if entryAt < 0 || extraAt < 0 || entryAt > extraAt {
				t.Errorf("entry must come before script args: %v", cmd.Args)
			}
		})
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func TestRuntimeString(t *testing.T) {
	t.Parallel()

	rt := Runtime{Kind: KindBun, Path: "/usr/bin/bun", Version: Version{1, 1, 20}}
	if got, want := rt.String(), "bun 1.1.20 (/usr/bin/bun)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Find must work against whatever is really installed. Skips cleanly when
// nothing is, so the suite stays green on a bare CI image.
func TestFindRealRuntime(t *testing.T) {
	t.Parallel()

	rt, err := Find(t.Context())
	if errors.Is(err, ErrNotFound) {
		t.Skip("no JavaScript runtime installed")
	}
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	t.Logf("detected %s", rt)

	if rt.Version.Less(minVersion[rt.Kind]) {
		t.Fatalf("%s is below its own floor %s", rt, minVersion[rt.Kind])
	}
}
