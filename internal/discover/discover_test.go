package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindLocatesModules(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "a", "package.json"),
		`{"name":"@app/a","jsingo":{"entry":"a.ts"}}`)
	write(t, filepath.Join(root, "a", "a.ts"), "")
	write(t, filepath.Join(root, "b", "deep", "package.json"),
		`{"name":"@app/b","jsingo":{"entry":"b.ts","out":"custom.js","target":"bun"}}`)
	// A package.json with no jsingo key is not a module.
	write(t, filepath.Join(root, "plain", "package.json"), `{"name":"plain"}`)

	mods, err := Find(root)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(mods) != 2 {
		t.Fatalf("found %d modules, want 2: %+v", len(mods), mods)
	}
	if mods[0].Name != "@app/a" || mods[1].Name != "@app/b" {
		t.Fatalf("wrong or unsorted results: %v, %v", mods[0].Name, mods[1].Name)
	}
	if got := filepath.Base(mods[1].OutPath()); got != "custom.js" {
		t.Errorf("out = %q, want the manifest override", got)
	}
	if mods[1].Target() != "bun" {
		t.Errorf("target = %q", mods[1].Target())
	}
}

// node_modules holds thousands of package.json files, every one of which would
// otherwise be walked. Skipping it is both correctness and speed.
func TestFindSkipsNodeModulesAndDotDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "js", "package.json"),
		`{"name":"real","jsingo":{"entry":"x.ts"}}`)
	write(t, filepath.Join(root, "js", "node_modules", "dep", "package.json"),
		`{"name":"dep","jsingo":{"entry":"evil.ts"}}`)
	write(t, filepath.Join(root, ".cache", "package.json"),
		`{"name":"cached","jsingo":{"entry":"y.ts"}}`)

	mods, err := Find(root)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(mods) != 1 || mods[0].Name != "real" {
		t.Fatalf("got %+v, want only the real module", mods)
	}
}

func TestDefaultsAndOutPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "js", "package.json"),
		`{"name":"m","jsingo":{"entry":"article.ts"}}`)

	mods, err := Find(root)
	if err != nil || len(mods) != 1 {
		t.Fatalf("Find: %v, %d modules", err, len(mods))
	}
	m := mods[0]

	if got := filepath.Base(m.OutPath()); got != "article.bundle.js" {
		t.Errorf("default out = %q", got)
	}
	// node output runs under bun; bun output does not run under node, so node
	// is the portable default.
	if m.Target() != "node" {
		t.Errorf("default target = %q, want node", m.Target())
	}
	if !strings.HasSuffix(m.EntryPath(), filepath.Join("js", "article.ts")) {
		t.Errorf("entry = %q", m.EntryPath())
	}
}

func TestMalformedManifestIsReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "js", "package.json"),
		`{"name":"m","jsingo":{"entry":123}}`)

	if _, err := Find(root); err == nil {
		t.Fatal("want an error for a malformed jsingo section")
	}
}

func TestMissingEntryIsReported(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "js", "package.json"), `{"name":"m","jsingo":{}}`)

	_, err := Find(root)
	if err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("got %v, want an error naming the missing entry", err)
	}
}

// A module with no dependencies needs no lockfile, so it must not be flagged.
func TestLockfileDetection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "withdeps", "package.json"),
		`{"name":"d","dependencies":{"x":"1.0.0"},"jsingo":{"entry":"a.ts"}}`)
	write(t, filepath.Join(root, "nodeps", "package.json"),
		`{"name":"n","jsingo":{"entry":"a.ts"}}`)
	write(t, filepath.Join(root, "locked", "package.json"),
		`{"name":"l","dependencies":{"x":"1.0.0"},"jsingo":{"entry":"a.ts"}}`)
	write(t, filepath.Join(root, "locked", "bun.lock"), "")

	mods, err := Find(root)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	byName := map[string]Module{}
	for _, m := range mods {
		byName[m.Name] = m
	}

	if byName["d"].HasLockfile {
		t.Error("a module with dependencies and no lockfile should be flagged")
	}
	if !byName["n"].HasLockfile {
		t.Error("a module with no dependencies needs no lockfile")
	}
	if !byName["l"].HasLockfile {
		t.Error("bun.lock was not detected")
	}
}
