package jsingo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DiyRex/jsingo/internal/detect"
	"github.com/DiyRex/jsingo/internal/hostsrc"
	"github.com/DiyRex/jsingo/internal/sandbox"
	"github.com/DiyRex/jsingo/internal/supervisor"
	"github.com/DiyRex/jsingo/internal/wire"
)

// Runtime owns one supervised sidecar process.
//
// It is safe for concurrent use. Calls from many goroutines are multiplexed
// over a single connection, and a sidecar crash is recovered transparently:
// in-flight calls fail with [ErrSidecarRestarting] and subsequent ones succeed
// once the replacement is up.
type Runtime struct {
	cfg *config
	rt  detect.Runtime

	sup *supervisor.Supervisor

	// mux is swapped on every reconnect. Callers load it per call rather than
	// holding a reference, so a restart does not strand them on a dead one.
	mux atomic.Pointer[wire.Mux]

	// ready is closed once the first session is serving.
	ready     chan struct{}
	readyOnce sync.Once

	// sem bounds concurrent calls.
	sem chan struct{}

	runDone  chan struct{}
	closeErr error
	closed   atomic.Bool
	stopped  atomic.Bool

	workDir string
	// entries are the module entrypoints handed to the host, one per module.
	entries []string

	calls  atomic.Int64
	failed atomic.Int64
}

// New starts a sidecar and waits for it to answer.
//
// It returns once the sidecar is serving, so a successful New means calls will
// work. Close must be called to release the process.
func New(ctx context.Context, opts ...Option) (*Runtime, error) {
	cfg := newConfig(opts)

	if len(cfg.modules) == 0 {
		return nil, errors.New("jsingo: no modules; pass at least one WithModule")
	}
	for _, m := range cfg.modules {
		if m.err != nil {
			return nil, m.err
		}
	}
	// Refuse credential-shaped names before anything starts, rather than
	// discovering the leak later.
	if err := cfg.sandbox.Validate(); err != nil {
		return nil, err
	}

	rt, err := resolveRuntime(ctx, cfg)
	if err != nil {
		return nil, err
	}

	lay, err := materialize(cfg)
	if err != nil {
		return nil, err
	}

	r := &Runtime{
		cfg:     cfg,
		rt:      rt,
		ready:   make(chan struct{}),
		runDone: make(chan struct{}),
		workDir: lay.dir,
		entries: lay.entries,
		sem:     make(chan struct{}, inFlightLimit(cfg)),
	}

	sup, err := supervisor.New(supervisor.Config{
		Spawn:         r.spawn,
		Connect:       r.serve,
		Logger:        cfg.logger,
		Backoff:       cfg.backoff,
		MaxRestarts:   cfg.maxRestarts,
		RestartWindow: cfg.restartWindow,
		ShutdownGrace: cfg.shutdownGrace,
	})
	if err != nil {
		return nil, err
	}
	r.sup = sup

	// The supervision loop outlives New; Close waits for it.
	go func() {
		defer close(r.runDone)
		if err := sup.Run(context.WithoutCancel(ctx)); err != nil {
			r.closeErr = err
			cfg.logger.Error("jsingo: supervisor stopped", "error", err)
		}
	}()

	if err := r.waitReady(ctx); err != nil {
		_ = r.Close(context.Background())
		return nil, err
	}
	go r.heartbeat()

	return r, nil
}

// resolveRuntime finds the JavaScript runtime to use.
func resolveRuntime(ctx context.Context, cfg *config) (detect.Runtime, error) {
	if cfg.runtimePath != "" {
		// An explicit path still gets a preference order for kind inference.
		kind := cfg.order[0]
		return detect.Runtime{Kind: kind, Path: cfg.runtimePath}, nil
	}
	rt, err := detect.Find(ctx, detect.WithOrder(cfg.order...))
	if err != nil {
		return detect.Runtime{}, fmt.Errorf("%w: %w", ErrNoRuntime, err)
	}
	return rt, nil
}

// inFlightLimit derives the concurrency cap.
//
// JavaScript is single-threaded, so beyond a small multiple of the CPU count
// extra concurrency only queues work inside the sidecar while holding memory
// on both sides.
func inFlightLimit(cfg *config) int {
	if cfg.maxInFlight > 0 {
		return cfg.maxInFlight
	}
	n := runtime.GOMAXPROCS(0) * 4
	if n < 8 {
		n = 8
	}
	return n
}

// materialize writes the host and module files to a content-addressed cache
// directory and returns that directory plus the entry argument for the host.
//
// Keying on content means identical modules are written once and shared across
// restarts and processes, while changed content can never reuse a stale
// directory.
func materialize(cfg *config) (*layout, error) {
	h := sha256.New()
	h.Write(hostsrc.Bundle)
	for _, m := range cfg.modules {
		mh, err := m.hash()
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(h, "%s:%s\x00", m.name, mh)
	}
	key := hex.EncodeToString(h.Sum(nil))[:32]

	base := cfg.cacheDir
	if base == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			userCache = os.TempDir()
		}
		base = filepath.Join(userCache, "jsingo")
	}
	dir := filepath.Join(base, key)

	// 0700: the extracted tree is executable code, and nothing outside this
	// user has any reason to read or modify it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jsingo: create cache dir: %w", err)
	}

	hostPath := filepath.Join(dir, hostsrc.Name)
	if err := writeIfChanged(hostPath, hostsrc.Bundle, 0o500); err != nil {
		return nil, err
	}

	entries := make([]string, 0, len(cfg.modules))
	for _, m := range cfg.modules {
		if err := extractModule(dir, m); err != nil {
			return nil, err
		}
		entries = append(entries, filepath.Join(dir, filepath.FromSlash(m.entry)))
	}
	return &layout{dir: dir, entries: entries}, nil
}

// layout is where the host and the module files were written.
type layout struct {
	dir     string
	entries []string
}

func extractModule(dir string, m *Mod) error {
	files, err := m.files()
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := fs.ReadFile(m.fsys, f)
		if err != nil {
			return fmt.Errorf("jsingo: read %q from module %q: %w", f, m.name, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("jsingo: create %q: %w", filepath.Dir(dst), err)
		}
		// Read-only: the sidecar executes these files and must not be able to
		// rewrite them, so a compromised dependency cannot persist itself.
		if err := writeIfChanged(dst, b, 0o400); err != nil {
			return err
		}
	}
	return nil
}

// writeIfChanged writes content only when the target differs.
//
// Rewriting on every start would defeat the cache and, worse, race with
// another process reading the same file. Files are written via a temporary
// name and renamed, so a reader never sees a partial file.
func writeIfChanged(path string, content []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && sameBytes(existing, content) {
		return nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("jsingo: stage %q: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("jsingo: write %q: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("jsingo: chmod %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("jsingo: close %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("jsingo: install %q: %w", path, err)
	}
	return nil
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// spawn builds the sidecar command. Called once per start and restart.
func (r *Runtime) spawn(ctx context.Context, childFD int) (*exec.Cmd, error) {
	hostPath := filepath.Join(r.workDir, hostsrc.Name)

	args := sandbox.HardenArgs(string(r.rt.Kind), r.cfg.maxHeapMB)
	// Every module entry is passed; the host imports each and merges their
	// exports into one method table.
	args = append(args, hostPath)
	args = append(args, r.entries...)
	cmd := exec.CommandContext(ctx, r.rt.Path, args...)

	policy := r.cfg.sandbox
	if policy.Dir == "" {
		policy.Dir = r.workDir
	}
	policy.Apply(cmd, map[string]string{
		"JSINGO_FD":      fmt.Sprint(childFD),
		"JSINGO_TIMEOUT": fmt.Sprint(r.cfg.heartbeatTimeout.Milliseconds()),
	})
	return cmd, nil
}

// serve runs one session over conn and returns when it ends.
func (r *Runtime) serve(_ context.Context, conn io.ReadWriteCloser) error {
	m := wire.NewMux(
		wire.NewReader(conn, r.cfg.maxFrameSize),
		wire.NewWriter(conn, r.cfg.maxFrameSize),
		wire.MuxOptions{
			Log:              r.onLog,
			MaxReplyBytes:    r.cfg.maxReplyBytes,
			LogRatePerSecond: wire.DefaultLogRate,
			LogBurst:         wire.DefaultLogBurst,
		},
	)
	r.mux.Store(m)
	r.readyOnce.Do(func() { close(r.ready) })

	err := m.Serve()

	// Clear the pointer so a call between sessions fails fast with
	// ErrSidecarRestarting rather than blocking on a dead connection.
	r.mux.CompareAndSwap(m, nil)
	return err
}

// onLog forwards a sidecar log record. Runs on the read goroutine, so it must
// not block.
func (r *Runtime) onLog(f wire.Frame) {
	r.cfg.logger.Info("jsingo sidecar", "record", string(f.Payload))
}

// waitReady blocks until the sidecar answers a ping or the deadline passes.
//
// Readiness is a successful round-trip, not the process existing: a runtime
// that started and then failed to load the bundle is not ready.
func (r *Runtime) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.startupWait)
	defer cancel()

	select {
	case <-r.ready:
	case <-r.runDone:
		if r.closeErr != nil {
			return translate("", r.closeErr)
		}
		return fmt.Errorf("%w: sidecar exited during startup", ErrSidecarUnrecoverable)
	case <-ctx.Done():
		return fmt.Errorf("jsingo: sidecar did not connect within %v: %w",
			r.cfg.startupWait, ctx.Err())
	}

	var lastErr error
	for {
		if m := r.mux.Load(); m != nil {
			pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
			err := m.Ping(pingCtx)
			pingCancel()
			if err == nil {
				return nil
			}
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jsingo: sidecar did not become ready within %v (last error: %v): %w",
				r.cfg.startupWait, lastErr, ctx.Err())
		case <-r.runDone:
			return fmt.Errorf("%w: sidecar exited during startup", ErrSidecarUnrecoverable)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// heartbeat pings the sidecar so its watchdog stays satisfied.
//
// Without this the sidecar exits on its own after the timeout - which is the
// point, since on macOS and the BSDs nothing else reaps it when the parent is
// killed with SIGKILL.
func (r *Runtime) heartbeat() {
	if r.cfg.heartbeatInterval <= 0 || r.cfg.heartbeatTimeout <= 0 {
		return
	}
	ticker := time.NewTicker(r.cfg.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.runDone:
			return
		case <-ticker.C:
			m := r.mux.Load()
			if m == nil {
				continue // restarting; the next tick will find the new session
			}
			ctx, cancel := context.WithTimeout(context.Background(), r.cfg.heartbeatInterval)
			if err := m.Ping(ctx); err != nil && !r.closed.Load() {
				r.cfg.logger.Debug("jsingo: heartbeat failed", "error", err)
			}
			cancel()
		}
	}
}

// call performs one round-trip. Used by Bind.
func (r *Runtime) call(ctx context.Context, method string, body []byte) ([]byte, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}

	// Bound concurrency before touching the connection, so queued callers wait
	// in Go rather than piling work into a single-threaded sidecar.
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	m := r.mux.Load()
	if m == nil {
		select {
		case <-r.runDone:
			if r.closeErr != nil {
				return nil, translate(method, r.closeErr)
			}
			return nil, ErrClosed
		default:
			return nil, fmt.Errorf("%w: no active session", ErrSidecarRestarting)
		}
	}

	r.calls.Add(1)
	out, err := m.Call(ctx, method, body)
	if err != nil {
		r.failed.Add(1)
		// A closed mux during a call means the sidecar died, unless we are the
		// ones shutting down.
		if errors.Is(err, wire.ErrClosed) && r.closed.Load() {
			return nil, ErrClosed
		}
		return nil, translate(method, err)
	}
	return out, nil
}

// Ping round-trips a liveness check. Suitable for a readiness probe.
func (r *Runtime) Ping(ctx context.Context) error {
	if r.closed.Load() {
		return ErrClosed
	}
	m := r.mux.Load()
	if m == nil {
		return fmt.Errorf("%w: no active session", ErrSidecarRestarting)
	}
	return translate("", m.Ping(ctx))
}

// Err reports a terminal failure, or nil while the Runtime is healthy.
//
// A non-nil result wrapping [ErrSidecarUnrecoverable] means the restart budget
// is exhausted and no retry will help. That, not a transient error, is what a
// liveness probe should fail on.
func (r *Runtime) Err() error {
	select {
	case <-r.runDone:
		if r.closeErr != nil {
			return translate("", r.closeErr)
		}
		return nil
	default:
		return nil
	}
}

// Stats is a point-in-time view of the Runtime.
type Stats struct {
	// Runtime describes the JavaScript runtime in use.
	Runtime string
	// Restarts counts sidecar respawns since New.
	Restarts int64
	// Uptime is how long the current sidecar process has been running.
	Uptime time.Duration
	// Calls and Failed count round-trips attempted and failed.
	Calls, Failed int64
	// InFlight is the number of calls awaiting a reply.
	InFlight int
	// Connected reports whether a session is currently established.
	Connected bool
}

// Stats returns current counters.
func (r *Runtime) Stats() Stats {
	s := Stats{
		Runtime:  r.rt.String(),
		Restarts: r.sup.Restarts(),
		Uptime:   r.sup.Uptime(),
		Calls:    r.calls.Load(),
		Failed:   r.failed.Load(),
	}
	if m := r.mux.Load(); m != nil {
		s.InFlight = m.InFlight()
		s.Connected = true
	}
	return s
}

// Close shuts the sidecar down and releases its resources.
//
// In-flight calls fail with [ErrClosed]. Close is idempotent and blocks until
// the process is reaped or ctx expires; returning before the process is gone
// would leave an orphan holding memory.
func (r *Runtime) Close(ctx context.Context) error {
	if r.closed.Swap(true) {
		<-r.runDone
		return nil
	}

	if m := r.mux.Load(); m != nil {
		_ = m.Close()
	}
	if !r.stopped.Swap(true) {
		r.sup.Stop()
	}

	select {
	case <-r.runDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jsingo: sidecar did not shut down before the deadline: %w", ctx.Err())
	}
}
