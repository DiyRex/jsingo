package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DiyRex/jsingo/internal/detect"
	"github.com/DiyRex/jsingo/internal/discover"
	"github.com/DiyRex/jsingo/internal/supervisor"
)

// runDoctor reports environment and configuration problems.
//
// Everything it checks is something that otherwise surfaces late and
// confusingly: a binary that is 30 MB too large, a build that is not
// reproducible, a sidecar that outlives its parent.
func runDoctor(ctx context.Context, log *logger, argv []string) error {
	fs := flagSet("doctor")
	root := fs.String("C", ".", "directory to inspect")
	strict := fs.Bool("strict", false, "treat warnings as failures")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	d := &doctor{log: log, root: *root}

	d.checkRuntimes(ctx)
	d.checkParentDeath()
	d.checkModules()

	switch {
	case d.problems > 0:
		return fmt.Errorf("%d problem(s) found", d.problems)
	case d.warnings > 0 && *strict:
		return fmt.Errorf("%d warning(s) found (strict mode)", d.warnings)
	case d.warnings > 0:
		log.warn("%d warning(s); nothing fatal", d.warnings)
		return nil
	default:
		log.ok("no problems found")
		return nil
	}
}

type doctor struct {
	log      *logger
	root     string
	problems int
	warnings int
}

func (d *doctor) fail(format string, args ...any) {
	d.problems++
	d.log.error(format, args...)
}

func (d *doctor) warn(format string, args ...any) {
	d.warnings++
	d.log.warn(format, args...)
}

func (d *doctor) checkRuntimes(ctx context.Context) {
	d.log.step("JavaScript runtimes")

	var found int
	for _, kind := range detect.DefaultOrder {
		rt, err := detect.Find(ctx, detect.WithOrder(kind))
		if err != nil {
			d.log.info("%-5s not available", kind)
			continue
		}
		found++
		d.log.ok("%s", rt)

		// Runtime choice has security consequences that are easy to miss.
		switch kind {
		case detect.KindBun:
			d.log.info("      bun ignores --disallow-code-generation-from-strings; eval stays reachable")
		case detect.KindDeno:
			d.log.info("      deno has enforced deny-by-default permissions - the strongest option")
		}
	}
	if found == 0 {
		d.fail("no JavaScript runtime found; install bun (https://bun.sh) or node 18+")
	}
}

func (d *doctor) checkParentDeath() {
	d.log.step("orphan protection")

	if supervisor.HasParentDeathSignal {
		d.log.ok("PR_SET_PDEATHSIG available; the kernel reaps the sidecar if this process is killed")
		return
	}
	d.log.ok("no PR_SET_PDEATHSIG on this platform; the heartbeat watchdog is the backstop")
	d.log.info("      do not disable the heartbeat here - nothing else reaps a sidecar after SIGKILL")
}

func (d *doctor) checkModules() {
	d.log.step("modules")

	mods, err := discover.Find(d.root)
	if err != nil {
		d.fail("discovery failed: %v", err)
		return
	}
	if len(mods) == 0 {
		d.warn("no jsingo modules found under %s", d.root)
		return
	}

	for _, m := range mods {
		d.log.info("%s (%s)", m.Name, rel(d.root, m.Dir))

		if _, err := os.Stat(m.EntryPath()); err != nil {
			d.fail("  %s: entry %q does not exist", m.Name, m.Manifest.Entry)
		}
		if !m.HasLockfile {
			d.warn("  %s: no committed lockfile; builds are not reproducible", m.Name)
		}
		if _, err := os.Stat(m.OutPath()); err != nil {
			d.warn("  %s: no bundle yet; run 'jsingo build'", m.Name)
		}
		if m.Target() == "bun" {
			d.warn("  %s: target \"bun\" does not run under node; use \"node\" for portability", m.Name)
		}
		d.checkEmbedHazard(m)
	}
}

// checkEmbedHazard is the reason doctor exists.
//
// //go:embed js recurses and skips only entries prefixed with "." or "_".
// node_modules matches neither, so a directory embed beside installed
// dependencies silently adds tens of megabytes to every binary - no error, no
// warning, and nothing in the build output that hints at it.
func (d *doctor) checkEmbedHazard(m discover.Module) {
	if !m.HasNodeModules {
		return
	}

	size, count := dirSize(filepath.Join(m.Dir, "node_modules"))
	dir := filepath.Base(m.Dir)

	goFiles, err := filepath.Glob(filepath.Join(filepath.Dir(m.Dir), "*.go"))
	if err != nil || len(goFiles) == 0 {
		return
	}

	for _, gf := range goFiles {
		b, err := os.ReadFile(gf)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "//go:embed ") {
				continue
			}
			for _, pat := range strings.Fields(strings.TrimPrefix(line, "//go:embed ")) {
				// A bare directory name, or "all:" on one, takes everything
				// below it. A pattern containing a separator or a wildcard is
				// specific enough to be safe.
				bare := strings.TrimPrefix(pat, "all:")
				if bare == dir || bare == "./"+dir {
					d.fail("  %s embeds the directory %q, which contains node_modules "+
						"(%s across %d files). Use explicit patterns such as "+
						"//go:embed %s/*.ts %s/package.json",
						rel(d.root, gf), pat, size, count, dir, dir)
				}
			}
		}
	}
}

func dirSize(root string) (string, int) {
	var total int64
	var count int
	_ = filepath.WalkDir(root, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not fatal here
		}
		count++
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return size(int(total)), count
}

var _ = flag.ErrHelp
