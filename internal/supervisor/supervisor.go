// Package supervisor owns the sidecar process lifecycle: spawning it, wiring
// its transport, restarting it after a crash, and guaranteeing it dies when
// the parent does.
//
// It knows nothing about the framing protocol. It hands a connected endpoint
// to a Connect callback and lets the layer above speak whatever it likes.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DiyRex/jsingo/internal/transport"
)

// ErrCrashLoop means the restart budget was exhausted; the sidecar is
// considered permanently broken and will not be respawned.
//
// A deliberate Stop is not an error, so it has no sentinel: Run returns nil.
var ErrCrashLoop = errors.New("supervisor: crash loop, giving up")

// Spawner starts the sidecar process. childFD is the descriptor number the
// child should read and write the protocol on.
type Spawner func(ctx context.Context, childFD int) (*exec.Cmd, error)

// Connect is called with a live endpoint each time the sidecar starts. It
// should block for the lifetime of that connection and return when it ends.
// The returned error, if any, is treated as the reason the session ended.
type Connect func(ctx context.Context, conn io.ReadWriteCloser) error

// Config configures a Supervisor.
type Config struct {
	// Spawn builds the process. Required.
	Spawn Spawner
	// Connect runs one session over the transport. Required.
	Connect Connect
	// Logger receives lifecycle events and the sidecar's stderr. Required.
	Logger *slog.Logger

	// Backoff shapes restart delays. The zero value is sensible.
	Backoff Backoff
	// MaxRestarts within RestartWindow before declaring a crash loop.
	// Zero selects DefaultMaxRestarts.
	MaxRestarts int
	// RestartWindow is the span over which MaxRestarts is counted.
	// Zero selects DefaultRestartWindow.
	RestartWindow time.Duration
	// ShutdownGrace is how long a child gets to exit after SIGTERM before it
	// is SIGKILLed. Zero selects DefaultShutdownGrace.
	ShutdownGrace time.Duration
}

// Supervisor defaults.
const (
	DefaultMaxRestarts    = 5
	DefaultRestartWindow  = 30 * time.Second
	DefaultShutdownGrace  = 5 * time.Second
	stderrLineLimit       = 64 << 10
	stderrTailForDiagnose = 3
)

// Supervisor keeps one sidecar process running.
//
// Run drives the supervision loop; Stop ends it. A Supervisor is single-use:
// once stopped it cannot be restarted.
type Supervisor struct {
	cfg Config

	stopOnce sync.Once
	stopped  chan struct{}

	restarts atomic.Int64
	// startedAt is the unix nano of the current process's start, or 0 if no
	// process is running.
	startedAt atomic.Int64
}

// New validates cfg and returns a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	switch {
	case cfg.Spawn == nil:
		return nil, errors.New("supervisor: Spawn is required")
	case cfg.Connect == nil:
		return nil, errors.New("supervisor: Connect is required")
	case cfg.Logger == nil:
		return nil, errors.New("supervisor: Logger is required")
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = DefaultMaxRestarts
	}
	if cfg.RestartWindow <= 0 {
		cfg.RestartWindow = DefaultRestartWindow
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = DefaultShutdownGrace
	}
	return &Supervisor{cfg: cfg, stopped: make(chan struct{})}, nil
}

// Run supervises the sidecar until ctx ends, Stop is called, or the restart
// budget is exhausted.
//
// It returns nil for a deliberate shutdown and ErrCrashLoop when the sidecar
// failed too often inside RestartWindow.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Stop must unblock a Run that is parked waiting on a session or a backoff
	// timer, so translate it into cancellation.
	go func() {
		select {
		case <-s.stopped:
			cancel()
		case <-ctx.Done():
		}
	}()

	// failures holds the start times of recent failed sessions, trimmed to
	// RestartWindow. Counting inside a sliding window rather than tracking a
	// total is what lets a long-lived process recover its budget instead of
	// dying from failures that happened hours apart.
	var failures []time.Time
	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			return s.exitReason(err)
		}

		start := time.Now()
		sessionErr := s.runOnce(ctx)
		lifetime := time.Since(start)

		if ctx.Err() != nil {
			return s.exitReason(ctx.Err())
		}
		if sessionErr == nil {
			// The sidecar closed the connection cleanly and nobody asked us to
			// stop. Treat that as a failure to stay up: a healthy sidecar
			// stays connected.
			sessionErr = errors.New("sidecar exited without being asked to")
		}

		failures = append(trimBefore(failures, start.Add(-s.cfg.RestartWindow)), start)
		if len(failures) > s.cfg.MaxRestarts {
			s.cfg.Logger.Error("sidecar crash loop; not restarting",
				"failures", len(failures),
				"window", s.cfg.RestartWindow,
				"last_error", sessionErr)
			return fmt.Errorf("%w: %d failures in %v: %w",
				ErrCrashLoop, len(failures), s.cfg.RestartWindow, sessionErr)
		}

		// A session that stayed up for the whole window was healthy; reset the
		// delay so an occasional crash days apart does not inherit a long
		// backoff from ancient history.
		if lifetime >= s.cfg.RestartWindow {
			attempt = 0
		}

		delay := s.cfg.Backoff.Delay(attempt)
		attempt++
		s.restarts.Add(1)

		s.cfg.Logger.Warn("sidecar exited; restarting",
			"error", sessionErr,
			"lifetime", lifetime.Round(time.Millisecond),
			"delay", delay.Round(time.Millisecond),
			"failures_in_window", len(failures))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return s.exitReason(ctx.Err())
		}
	}
}

// exitReason distinguishes a deliberate Stop from the caller's context ending.
func (s *Supervisor) exitReason(ctxErr error) error {
	select {
	case <-s.stopped:
		return nil // Stop was called: an orderly shutdown, not a failure.
	default:
		return ctxErr
	}
}

// runOnce spawns the sidecar, runs one session, and guarantees the process is
// reaped before returning.
func (s *Supervisor) runOnce(ctx context.Context) (err error) {
	pair, err := transport.NewPair()
	if err != nil {
		return err
	}
	// On every path out of this function the local endpoint must be closed, or
	// the descriptor leaks once per restart.
	defer func() {
		if cerr := pair.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	cmd, err := s.cfg.Spawn(ctx, transport.ChildFD)
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// ExtraFiles[0] becomes descriptor 3 in the child, which is
	// transport.ChildFD.
	cmd.ExtraFiles = append(cmd.ExtraFiles, pair.Child)
	configureProcAttr(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	s.startedAt.Store(time.Now().UnixNano())
	defer s.startedAt.Store(0)

	// The parent's copy of the child's endpoint must go now. While it is open
	// the socket stays alive from the kernel's point of view, so a sidecar
	// crash would never surface as EOF and the session would hang.
	if err := pair.CloseChild(); err != nil {
		s.terminate(cmd)
		return err
	}

	logDone := make(chan []string, 1)
	go func() { logDone <- s.pumpStderr(cmd.Process.Pid, stderr) }()

	s.cfg.Logger.Info("sidecar started", "pid", cmd.Process.Pid)

	sessionErr := s.cfg.Connect(ctx, pair.Local)

	// Close our endpoint before waiting: a sidecar blocked reading the socket
	// needs the EOF to notice it should exit, and without it Wait would block
	// until the grace period expired on every single restart.
	if cerr := pair.Local.Close(); cerr != nil && sessionErr == nil {
		sessionErr = cerr
	}
	pair.Local = nil

	waitErr := s.terminate(cmd)
	tail := <-logDone

	return sessionFailure(sessionErr, waitErr, tail)
}

// sessionFailure combines what the session saw, why the process exited, and
// what it printed, into the single most useful error.
//
// Precedence matters. When a sidecar dies, Connect observes only EOF, while
// the real cause - a bad exit status and a stack trace on stderr - is in the
// other two. Letting the EOF win would report "EOF" for a syntax error in the
// bundle. Conversely a genuine protocol violation is more informative than the
// exit status, so it is kept and the exit detail joined to it.
//
// The stderr tail is attached in every case: for a sidecar that dies before it
// can send a LOG frame, it is the only diagnostic that exists.
func sessionFailure(sessionErr, waitErr error, tail []string) error {
	var primary error
	switch {
	case sessionErr == nil:
		primary = waitErr
	case isPeerGone(sessionErr) && waitErr != nil:
		primary = waitErr
	case waitErr != nil && !isPeerGone(sessionErr):
		primary = errors.Join(sessionErr, waitErr)
	default:
		primary = sessionErr
	}
	return annotateExit(primary, tail)
}

// isPeerGone reports whether an error only says "the other end went away",
// carrying no information about why.
func isPeerGone(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// terminate ends the child and reaps it, escalating SIGTERM to SIGKILL.
//
// The escalation matters: a sidecar wedged in a synchronous npm call cannot
// service a signal handler, and without the SIGKILL the supervisor would block
// on Wait forever.
func (s *Supervisor) terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	// Signal the group, not the process, so a JS worker thread or a spawned
	// grandchild does not survive as an orphan.
	if err := signalGroup(pid, syscall.SIGTERM); err != nil && !isProcessGone(err) {
		s.cfg.Logger.Debug("SIGTERM to process group failed", "pid", pid, "error", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(s.cfg.ShutdownGrace):
		s.cfg.Logger.Warn("sidecar ignored SIGTERM; sending SIGKILL",
			"pid", pid, "grace", s.cfg.ShutdownGrace)
		if err := signalGroup(pid, syscall.SIGKILL); err != nil && !isProcessGone(err) {
			s.cfg.Logger.Error("SIGKILL failed", "pid", pid, "error", err)
		}
		return <-done
	}
}

// pumpStderr forwards the sidecar's stderr to the logger and returns the last
// few lines.
//
// Those lines are the whole diagnostic story for a sidecar that dies before it
// can send a LOG frame - a syntax error in the bundle, a missing native
// module - so they are attached to the exit error rather than only logged.
func (s *Supervisor) pumpStderr(pid int, r io.Reader) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8<<10), stderrLineLimit)

	tail := make([]string, 0, stderrTailForDiagnose)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.cfg.Logger.Info("sidecar stderr", "pid", pid, "line", line)

		if len(tail) == stderrTailForDiagnose {
			tail = tail[1:]
		}
		tail = append(tail, line)
	}
	// A scan error is almost always the pipe closing as the process exits,
	// which is expected. The one case worth surfacing is a line longer than
	// the buffer, because that silently truncates diagnostics.
	if err := sc.Err(); err != nil && errors.Is(err, bufio.ErrTooLong) {
		s.cfg.Logger.Warn("sidecar stderr line exceeded limit; output truncated",
			"pid", pid, "limit", stderrLineLimit)
	}
	return tail
}

// Stop shuts the supervisor down. It is idempotent and safe to call
// concurrently with Run.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// Restarts reports how many times the sidecar has been restarted.
func (s *Supervisor) Restarts() int64 { return s.restarts.Load() }

// Uptime reports how long the current sidecar process has been running, or
// zero if none is.
func (s *Supervisor) Uptime() time.Duration {
	ns := s.startedAt.Load()
	if ns == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ns))
}

// trimBefore drops timestamps at or before cutoff. The slice is ordered, so
// the first survivor ends the scan.
func trimBefore(ts []time.Time, cutoff time.Time) []time.Time {
	for i, t := range ts {
		if t.After(cutoff) {
			return ts[i:]
		}
	}
	return ts[:0]
}

// annotateExit attaches captured stderr to a process exit error.
func annotateExit(err error, tail []string) error {
	if err == nil || len(tail) == 0 {
		return err
	}
	return fmt.Errorf("%w (stderr: %s)", err, joinLines(tail))
}

func joinLines(lines []string) string {
	return strings.Join(lines, " | ")
}

// isProcessGone reports whether a signal failed only because the target had
// already exited, which is a race we expect rather than a fault.
func isProcessGone(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
