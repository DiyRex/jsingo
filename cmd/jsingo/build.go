package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/DiyRex/jsingo/internal/detect"
	"github.com/DiyRex/jsingo/internal/discover"
)

func runBuild(ctx context.Context, log *logger, argv []string) error {
	fs := flagSet("build")
	root := fs.String("C", ".", "directory to search for modules")
	install := fs.Bool("install", true, "install dependencies before bundling")
	minify := fs.Bool("minify", true, "minify the bundle")
	sourcemap := fs.Bool("sourcemap", false, "emit an external source map")
	check := fs.Bool("check", false, "fail if any bundle is out of date instead of writing it")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-module timeout")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	mods, err := discover.Find(*root)
	if err != nil {
		return err
	}
	if len(mods) == 0 {
		return fmt.Errorf("no jsingo modules found under %s\n"+
			"a module is a directory whose package.json has a top-level \"jsingo\" key, "+
			"for example: \"jsingo\": {\"entry\": \"article.ts\"}", *root)
	}

	bundler, err := detect.Find(ctx, detect.WithOrder(detect.KindBun))
	if err != nil {
		return fmt.Errorf("bundling needs bun: %w", err)
	}
	log.step("using %s", bundler)

	var stale []string
	for _, m := range mods {
		log.step("module %s", m.Name)

		if err := checkHazards(log, m); err != nil {
			return err
		}

		mctx, cancel := context.WithTimeout(ctx, *timeout)
		err := buildModule(mctx, log, bundler, m, buildOpts{
			install:   *install,
			minify:    *minify,
			sourcemap: *sourcemap,
			check:     *check,
		})
		cancel()

		var outdated *errOutOfDate
		if errors.As(err, &outdated) {
			stale = append(stale, outdated.path)
			log.warn("out of date: %s", rel(*root, outdated.path))
			continue
		}
		if err != nil {
			return fmt.Errorf("module %s: %w", m.Name, err)
		}
	}

	if len(stale) > 0 {
		return fmt.Errorf("%d bundle(s) out of date; run 'jsingo build' and commit the result", len(stale))
	}
	log.ok("%d module(s) built", len(mods))
	return nil
}

type buildOpts struct {
	install, minify, sourcemap, check bool
}

// errOutOfDate reports a stale bundle under -check.
type errOutOfDate struct{ path string }

func (e *errOutOfDate) Error() string { return "bundle out of date: " + e.path }

func buildModule(ctx context.Context, log *logger, bundler detect.Runtime, m discover.Module, opts buildOpts) error {
	if _, err := os.Stat(m.EntryPath()); err != nil {
		return fmt.Errorf("entry %q not found: %w", m.Manifest.Entry, err)
	}

	if opts.install && m.HasNodeModules || opts.install && hasDependencies(m) {
		if err := installDeps(ctx, log, bundler, m); err != nil {
			return err
		}
	}

	// Bundle to a temporary file first, so a failed build never leaves a
	// half-written bundle that go:embed would happily compile in.
	tmp, err := os.CreateTemp(m.Dir, ".jsingo-bundle-*.js")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()

	args := []string{
		"build", m.EntryPath(),
		"--target", m.Target(),
		"--format", "esm",
		"--outfile", tmpName,
	}
	if opts.minify {
		args = append(args, "--minify")
	}
	if opts.sourcemap {
		args = append(args, "--sourcemap=external")
	}
	for _, ext := range m.Manifest.External {
		args = append(args, "--external", ext)
	}

	cmd := exec.CommandContext(ctx, bundler.Path, args...)
	cmd.Dir = m.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bundle failed: %w\n%s", err, out)
	}

	built, err := os.ReadFile(tmpName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(built)

	out := m.OutPath()
	if existing, err := os.ReadFile(out); err == nil && sha256.Sum256(existing) == sum {
		log.ok("unchanged: %s (%s, %s)", filepath.Base(out), size(len(built)), shortHash(sum[:]))
		return nil
	}
	if opts.check {
		return &errOutOfDate{path: out}
	}

	if err := os.Rename(tmpName, out); err != nil {
		return fmt.Errorf("install bundle: %w", err)
	}
	// The bundle is committed build output, so it must be readable by anyone
	// who checks the repo out.
	if err := os.Chmod(out, 0o644); err != nil {
		return err
	}
	log.ok("wrote %s (%s, %s)", filepath.Base(out), size(len(built)), shortHash(sum[:]))
	return nil
}

func installDeps(ctx context.Context, log *logger, bundler detect.Runtime, m discover.Module) error {
	args := []string{"install", "--ignore-scripts"}
	if m.HasLockfile {
		args = append(args, "--frozen-lockfile")
	}

	// --ignore-scripts is not optional. npm lifecycle hooks run arbitrary code
	// at install time, on this machine, outside every runtime sandbox - the
	// most exploited vector in the ecosystem. Nothing jsingo builds needs them.
	log.info("bun %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, bundler.Path, args...)
	cmd.Dir = m.Dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install failed: %w\n%s", err, out)
	}
	return nil
}

// checkHazards refuses configurations that would produce a broken or bloated
// binary, rather than letting them through to be discovered later.
func checkHazards(log *logger, m discover.Module) error {
	if !m.HasLockfile {
		log.warn("%s has dependencies but no committed lockfile; builds are not reproducible", m.Name)
	}

	// The bundle must not land inside a directory that go:embed would pull
	// wholesale, and the entry must not be under node_modules.
	if strings.Contains(m.OutPath(), string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
		return fmt.Errorf("output path is inside node_modules: %s", m.OutPath())
	}
	return nil
}

func hasDependencies(m discover.Module) bool {
	b, err := os.ReadFile(filepath.Join(m.Dir, "package.json"))
	return err == nil && strings.Contains(string(b), "\"dependencies\"")
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func shortHash(sum []byte) string { return hex.EncodeToString(sum)[:12] }

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}
