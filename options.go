package jsingo

import (
	"log/slog"
	"time"

	"github.com/DiyRex/jsingo/internal/detect"
	"github.com/DiyRex/jsingo/internal/sandbox"
	"github.com/DiyRex/jsingo/internal/supervisor"
)

// RuntimeKind names a JavaScript runtime.
type RuntimeKind = detect.Kind

// Supported runtimes. Auto resolves bun, then node, then deno.
const (
	Bun  = detect.KindBun
	Node = detect.KindNode
	Deno = detect.KindDeno
)

// Option configures a Runtime. Functional options keep the constructor stable:
// a new knob does not change any existing call.
type Option func(*config)

type config struct {
	modules []*Mod
	codec   Codec
	logger  *slog.Logger

	order       []RuntimeKind
	runtimePath string

	sandbox     sandbox.Policy
	maxHeapMB   int
	startupWait time.Duration

	backoff       supervisor.Backoff
	maxRestarts   int
	restartWindow time.Duration
	shutdownGrace time.Duration

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration

	maxFrameSize  uint32
	maxReplyBytes int
	maxInFlight   int

	cacheDir string
}

// Defaults applied when the corresponding option is not given.
const (
	DefaultStartupWait       = 30 * time.Second
	DefaultHeartbeatInterval = 3 * time.Second
	DefaultHeartbeatTimeout  = 10 * time.Second
)

func newConfig(opts []Option) *config {
	c := &config{
		codec:             defaultCodec,
		logger:            slog.New(slog.DiscardHandler),
		order:             detect.DefaultOrder,
		startupWait:       DefaultStartupWait,
		heartbeatInterval: DefaultHeartbeatInterval,
		heartbeatTimeout:  DefaultHeartbeatTimeout,
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

// WithModule adds JavaScript modules to the sidecar.
//
// Several modules share one sidecar process: two npm libraries do not mean two
// runtimes. Their exports are callable by bare name, and by "module:name" when
// two modules export the same name.
func WithModule(mods ...*Mod) Option {
	return func(c *config) { c.modules = append(c.modules, mods...) }
}

// WithRuntime restricts which JavaScript runtimes may be used, in preference
// order. The default tries bun, then node, then deno.
//
// This is a security decision as well as a performance one: node honours
// --disallow-code-generation-from-strings while bun does not, and deno is the
// only one with enforced deny-by-default permissions. See docs/SECURITY.md.
func WithRuntime(kinds ...RuntimeKind) Option {
	return func(c *config) {
		if len(kinds) > 0 {
			c.order = kinds
		}
	}
}

// WithRuntimePath uses a specific runtime binary, skipping discovery.
func WithRuntimePath(path string) Option {
	return func(c *config) { c.runtimePath = path }
}

// WithLogger directs sidecar logs and lifecycle events to a logger.
//
// Without it the Runtime is silent: a library must not write to a program's
// output uninvited.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithCodec replaces the serialisation format. The sidecar must agree.
func WithCodec(codec Codec) Option {
	return func(c *config) {
		if codec != nil {
			c.codec = codec
		}
	}
}

// WithSandbox sets the isolation policy for the sidecar process.
//
// The default forwards no environment at all. Override it only to allow named
// variables through; see [Env] for the common case.
func WithSandbox(p sandbox.Policy) Option {
	return func(c *config) { c.sandbox = p }
}

// WithAllowedEnv forwards named environment variables to the sidecar.
//
// Everything else is dropped. Names that look like credentials are refused by
// [New] rather than forwarded, because the sidecar runs third-party npm code.
func WithAllowedEnv(names ...string) Option {
	return func(c *config) { c.sandbox.AllowEnv = append(c.sandbox.AllowEnv, names...) }
}

// WithEnv sets environment variables on the sidecar explicitly.
//
// This is the supported way to pass a value the sidecar genuinely needs -
// preferably a narrowly-scoped one, since any npm dependency in the bundle can
// read it.
func WithEnv(env map[string]string) Option {
	return func(c *config) {
		if c.sandbox.Env == nil {
			c.sandbox.Env = make(map[string]string, len(env))
		}
		for k, v := range env {
			c.sandbox.Env[k] = v
		}
	}
}

// WithMaxHeapMB caps the sidecar's JavaScript heap where the runtime supports
// it.
//
// This is cooperative, not enforced. The real limit is the container's cgroup;
// this only makes the runtime try to stay under it and fail sooner if it
// cannot.
func WithMaxHeapMB(mb int) Option {
	return func(c *config) { c.maxHeapMB = mb }
}

// WithStartupTimeout bounds how long New waits for the sidecar to answer.
func WithStartupTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.startupWait = d
		}
	}
}

// WithRestartPolicy configures crash recovery.
//
// More than max failures inside window stops the restarts and marks the
// Runtime unrecoverable. Counting within a sliding window lets a long-lived
// process recover its budget rather than dying from crashes hours apart.
func WithRestartPolicy(max int, window time.Duration) Option {
	return func(c *config) {
		c.maxRestarts = max
		c.restartWindow = window
	}
}

// WithBackoff shapes the delay between restart attempts.
func WithBackoff(minDelay, maxDelay time.Duration) Option {
	return func(c *config) {
		c.backoff.Min = minDelay
		c.backoff.Max = maxDelay
	}
}

// WithShutdownGrace sets how long the sidecar has to exit after SIGTERM before
// it is killed.
//
// In Kubernetes, terminationGracePeriodSeconds must exceed this, or the
// kubelet tears the container down mid-escalation.
func WithShutdownGrace(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.shutdownGrace = d
		}
	}
}

// WithHeartbeat tunes the liveness ping.
//
// The sidecar exits on its own if pings stop, which is the only thing that
// reaps it when the Go process is killed with SIGKILL on a platform without
// PR_SET_PDEATHSIG - macOS and the BSDs. Setting timeout to zero disables the
// watchdog and should be reserved for debugging.
func WithHeartbeat(interval, timeout time.Duration) Option {
	return func(c *config) {
		c.heartbeatInterval = interval
		c.heartbeatTimeout = timeout
	}
}

// WithMaxFrameSize caps a single protocol frame.
//
// It bounds what a compromised sidecar can make the Go process allocate in one
// go, so it should be set to the largest legitimate payload and no more.
func WithMaxFrameSize(n uint32) Option {
	return func(c *config) { c.maxFrameSize = n }
}

// WithMaxReplyBytes caps a single reply payload.
//
// Separate from WithMaxFrameSize because the two limits protect different
// things. Requests are legitimately large - a document to parse - while
// replies usually are not, and a single shared limit lets a tiny request
// elicit a maximal reply from a compromised sidecar.
func WithMaxReplyBytes(n int) Option {
	return func(c *config) { c.maxReplyBytes = n }
}

// WithMaxInFlight caps concurrent calls to the sidecar.
//
// JavaScript is single-threaded: past a point, more concurrent calls only
// queue inside the sidecar while consuming memory on both sides. Zero selects
// a default derived from GOMAXPROCS.
func WithMaxInFlight(n int) Option {
	return func(c *config) { c.maxInFlight = n }
}

// WithCacheDir sets where module files are extracted.
//
// The default is under os.UserCacheDir. Extraction is keyed by content hash,
// so identical content is written once and reused across restarts and
// processes.
func WithCacheDir(dir string) Option {
	return func(c *config) { c.cacheDir = dir }
}
