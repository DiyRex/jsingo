// Package sandbox reduces what the sidecar process can reach.
//
// The sidecar executes third-party npm code. Treat it as hostile: a
// transitive dependency you have never heard of runs with whatever authority
// the process is given, and npm supply-chain compromises are routine rather
// than hypothetical.
//
// Running JavaScript in a separate process is what makes this possible. An
// in-process engine such as goja or v8go shares the Go program's memory,
// environment, descriptors and credentials, and none of the controls here
// could exist. The process boundary is the security feature; this package
// spends it.
//
// # Layers
//
// This package covers what a Go parent can enforce portably: the environment
// the child sees, the runtime's own hardening flags, and its working
// directory. Memory, CPU and network confinement belong to the container
// runtime, which enforces them with cgroups and namespaces rather than
// cooperation. See docs/SECURITY.md; the Dockerfile and deploy manifests carry
// the other half, and neither half is sufficient alone.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Policy describes what a sidecar process is allowed to see.
//
// The zero value is the most restrictive setting: no inherited environment at
// all. That is deliberate. A permissive default would silently hand
// AWS_SECRET_ACCESS_KEY, DATABASE_URL and every other parent variable to
// third-party code, and nobody would notice until it was exfiltrated.
type Policy struct {
	// AllowEnv lists environment variable names to pass through from the
	// parent. Everything absent from this list is dropped.
	//
	// Names are matched exactly; there is no pattern support, because a
	// pattern like "AWS_*" is how a credential ends up forwarded by accident.
	AllowEnv []string

	// Env sets variables explicitly. These override anything from AllowEnv.
	Env map[string]string

	// Dir is the child's working directory. Empty means the parent's.
	Dir string

	// AllowParentEnv disables environment scrubbing entirely.
	//
	// This exists for local debugging. Setting it in production gives every
	// npm dependency in the bundle read access to every secret the parent
	// holds.
	AllowParentEnv bool
}

// baseEnv is the minimum a JavaScript runtime needs to start.
//
// Deliberately tiny. HOME points at the working directory rather than the
// real one, because $HOME is where credential files live: ~/.aws/credentials,
// ~/.ssh/id_rsa, ~/.npmrc, ~/.kube/config. A dependency that reads $HOME and
// walks it should find nothing.
func baseEnv(dir string) map[string]string {
	if dir == "" {
		dir = os.TempDir()
	}
	return map[string]string{
		// A minimal PATH. The runtime is launched by absolute path, so this
		// exists only for tooling that expects the variable to be set.
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"HOME": dir,
		// Keep temporary files inside the sandbox directory.
		"TMPDIR": dir,
		// Deterministic text handling regardless of the host locale.
		"LANG":   "C.UTF-8",
		"LC_ALL": "C.UTF-8",
		// Signals to npm packages that this is not a developer machine.
		"NODE_ENV": "production",
		"CI":       "1",
		// Neither runtime should phone home from inside a sandbox.
		"DO_NOT_TRACK":               "1",
		"BUN_INSTALL_CACHE_DIR":      dir,
		"NEXT_TELEMETRY_DISABLED":    "1",
		"npm_config_update_notifier": "false",
	}
}

// SensitiveEnvPrefixes are refused by AllowEnv.
//
// Allowlisting is already the safe direction, but an allowlist is written by a
// human under deadline pressure, and "just add AWS_PROFILE" is how a
// credential reaches an npm dependency. Refusing loudly at construction beats
// discovering it in an incident review.
var SensitiveEnvPrefixes = []string{
	"AWS_", "AZURE_", "GOOGLE_", "GCP_", "GITHUB_TOKEN", "GH_TOKEN",
	"DATABASE_", "DB_PASSWORD", "POSTGRES_", "MYSQL_", "REDIS_",
	"SECRET", "PASSWORD", "PASSWD", "TOKEN", "API_KEY", "APIKEY",
	"PRIVATE_KEY", "SSH_", "GPG_", "NPM_TOKEN", "KUBERNETES_",
	"VAULT_", "STRIPE_", "TWILIO_", "SENTRY_DSN",
}

// ErrSensitiveEnv reports an AllowEnv entry that looks like a credential.
type ErrSensitiveEnv struct {
	Name   string
	Prefix string
}

func (e *ErrSensitiveEnv) Error() string {
	return fmt.Sprintf(
		"sandbox: refusing to forward %q to the sidecar: it matches the sensitive pattern %q. "+
			"The sidecar runs third-party npm code; if it genuinely needs this value, "+
			"pass a narrowly-scoped copy through Policy.Env instead",
		e.Name, e.Prefix)
}

// Validate reports whether the policy forwards anything obviously sensitive.
func (p Policy) Validate() error {
	for _, name := range p.AllowEnv {
		upper := strings.ToUpper(name)
		for _, prefix := range SensitiveEnvPrefixes {
			if strings.Contains(upper, prefix) {
				return &ErrSensitiveEnv{Name: name, Prefix: prefix}
			}
		}
	}
	return nil
}

// Environ builds the child's environment.
//
// Precedence, lowest to highest: the minimal base, then AllowEnv pass-through,
// then Policy.Env. extra is applied last and is for values the caller must
// set, such as the protocol descriptor number.
func (p Policy) Environ(extra map[string]string) []string {
	if p.AllowParentEnv {
		out := os.Environ()
		for k, v := range extra {
			out = append(out, k+"="+v)
		}
		return out
	}

	env := baseEnv(p.Dir)

	for _, name := range p.AllowEnv {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Sorted so a process listing is stable and diffable between runs.
	sort.Strings(out)
	return out
}

// Apply configures cmd according to the policy.
//
// It sets the environment and working directory. It deliberately does not
// touch cmd.SysProcAttr, which the supervisor owns for process-group setup.
func (p Policy) Apply(cmd *exec.Cmd, extra map[string]string) {
	cmd.Env = p.Environ(extra)
	if p.Dir != "" {
		cmd.Dir = p.Dir
	}

	// The child must not inherit the parent's stdin. A dependency that reads
	// it would consume the parent's input, and there is nothing legitimate for
	// it to find there.
	cmd.Stdin = nil

	// stdout is discarded rather than forwarded. The protocol lives on fd 3, so
	// nothing of value is written here - only console.log from npm packages,
	// which must never be able to flood the parent's own output.
	//
	// Leaving it nil is what discards it: exec.Cmd connects a nil Stdout to
	// the null device itself. Do not "improve" this to os.NewFile(0,
	// os.DevNull) - that does not open /dev/null, it wraps descriptor 0, the
	// parent's stdin, and merely labels it. The child then writes to the
	// parent's stdin and closing it corrupts the parent's descriptor.
	cmd.Stdout = nil
}

// HardenArgs returns extra runtime flags that reduce the sidecar's capability.
//
// These are the runtime's own switches, so they are cooperative rather than
// enforced: code already running inside the runtime may be able to undo some
// of them. They raise the cost of an attack and are worth setting, but the
// enforcement boundary is the container, not these flags.
func HardenArgs(kind string, maxHeapMB int) []string {
	switch kind {
	case "node":
		args := []string{
			// Removes eval and new Function. A large fraction of obfuscated
			// npm payloads stage themselves through one of the two.
			"--disallow-code-generation-from-strings",
			// No implicit loading of code from $NODE_OPTIONS or the cwd.
			"--no-experimental-fetch",
		}
		if maxHeapMB > 0 {
			args = append(args, fmt.Sprintf("--max-old-space-size=%d", maxHeapMB))
		}
		return args

	case "bun":
		// --smol lowers heap growth aggressiveness, which suits a sidecar and
		// caps runaway growth sooner.
		return []string{"--smol"}

	case "deno":
		// Deno is the only runtime of the three with real, enforced,
		// deny-by-default permissions. Where a deployment can choose the
		// runtime, this is the strongest option.
		return []string{"run", "--no-prompt", "--deny-net", "--deny-env", "--deny-run", "--deny-ffi"}

	default:
		return nil
	}
}
