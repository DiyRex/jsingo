package sandbox

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func envMap(t *testing.T, entries []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", e)
		}
		m[k] = v
	}
	return m
}

// The default must not forward the parent's environment. This is the control
// that stops a compromised npm dependency reading the process's secrets.
func TestZeroPolicyDropsParentEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "super-secret")
	t.Setenv("DATABASE_URL", "postgres://user:pw@host/db")
	t.Setenv("MY_APP_SETTING", "harmless")

	env := envMap(t, Policy{}.Environ(nil))

	for _, leaked := range []string{"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "MY_APP_SETTING"} {
		if v, ok := env[leaked]; ok {
			t.Errorf("%s leaked to the sidecar with value %q", leaked, v)
		}
	}
}

func TestBaseEnvironmentIsMinimalAndSafe(t *testing.T) {
	t.Parallel()

	env := envMap(t, Policy{Dir: "/sandbox/work"}.Environ(nil))

	// HOME must not point at the real home directory: ~/.aws/credentials,
	// ~/.ssh and ~/.npmrc all live there.
	if env["HOME"] != "/sandbox/work" {
		t.Errorf("HOME = %q, want the sandbox directory", env["HOME"])
	}
	if env["TMPDIR"] != "/sandbox/work" {
		t.Errorf("TMPDIR = %q, want the sandbox directory", env["TMPDIR"])
	}
	if env["NODE_ENV"] != "production" {
		t.Errorf("NODE_ENV = %q", env["NODE_ENV"])
	}
	if strings.Contains(env["PATH"], "/usr/local/sbin") {
		t.Errorf("PATH is wider than intended: %q", env["PATH"])
	}
}

func TestAllowEnvPassesThroughNamedVariables(t *testing.T) {
	t.Setenv("FEATURE_FLAG", "on")
	t.Setenv("NOT_ALLOWED", "nope")

	env := envMap(t, Policy{AllowEnv: []string{"FEATURE_FLAG"}}.Environ(nil))

	if env["FEATURE_FLAG"] != "on" {
		t.Errorf("FEATURE_FLAG = %q, want %q", env["FEATURE_FLAG"], "on")
	}
	if _, ok := env["NOT_ALLOWED"]; ok {
		t.Error("NOT_ALLOWED was forwarded despite not being listed")
	}
}

func TestAllowEnvIgnoresUnsetVariables(t *testing.T) {
	t.Parallel()

	env := envMap(t, Policy{AllowEnv: []string{"DEFINITELY_NOT_SET_12345"}}.Environ(nil))
	if _, ok := env["DEFINITELY_NOT_SET_12345"]; ok {
		t.Error("an unset variable should not appear as empty")
	}
}

func TestPrecedenceExtraBeatsEnvBeatsAllowBeatsBase(t *testing.T) {
	t.Setenv("LAYERED", "from-parent")

	p := Policy{
		AllowEnv: []string{"LAYERED"},
		Env:      map[string]string{"LAYERED": "from-policy", "NODE_ENV": "test"},
	}
	env := envMap(t, p.Environ(map[string]string{"LAYERED": "from-extra"}))

	if env["LAYERED"] != "from-extra" {
		t.Errorf("LAYERED = %q, want the extra value to win", env["LAYERED"])
	}
	if env["NODE_ENV"] != "test" {
		t.Errorf("NODE_ENV = %q, want Policy.Env to override the base", env["NODE_ENV"])
	}
}

func TestValidateRejectsCredentialShapedNames(t *testing.T) {
	t.Parallel()

	tests := []string{
		"AWS_SECRET_ACCESS_KEY",
		"AWS_PROFILE",
		"DATABASE_URL",
		"GITHUB_TOKEN",
		"MY_API_KEY",
		"stripe_secret",    // case-insensitive
		"SERVICE_PASSWORD", // substring match
		"KUBERNETES_SERVICE_HOST",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := Policy{AllowEnv: []string{name}}.Validate()

			var sensitive *ErrSensitiveEnv
			if !errors.As(err, &sensitive) {
				t.Fatalf("Validate() = %v, want ErrSensitiveEnv", err)
			}
			// The message has to tell the operator what to do instead.
			if !strings.Contains(err.Error(), "Policy.Env") {
				t.Errorf("error gives no remedy: %v", err)
			}
		})
	}
}

func TestValidateAcceptsOrdinaryNames(t *testing.T) {
	t.Parallel()

	p := Policy{AllowEnv: []string{"FEATURE_FLAG", "LOG_LEVEL", "TZ", "APP_REGION"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// Policy.Env is the documented escape hatch, so it must not be blocked -
// passing a narrowly-scoped value deliberately is the behaviour we want.
func TestValidateDoesNotBlockExplicitEnv(t *testing.T) {
	t.Parallel()

	p := Policy{Env: map[string]string{"API_KEY": "scoped-read-only-key"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for an explicit value", err)
	}
}

func TestAllowParentEnvIsAnExplicitOptOut(t *testing.T) {
	t.Setenv("SOME_PARENT_VAR", "visible")

	env := envMap(t, Policy{AllowParentEnv: true}.Environ(map[string]string{"JSINGO_FD": "3"}))

	if env["SOME_PARENT_VAR"] != "visible" {
		t.Error("AllowParentEnv should forward the parent environment")
	}
	if env["JSINGO_FD"] != "3" {
		t.Error("extra values must still be applied")
	}
}

func TestApplySetsEnvAndDirAndClosesStdin(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("/bin/echo")
	Policy{Dir: "/tmp"}.Apply(cmd, map[string]string{"JSINGO_FD": "3"})

	if cmd.Dir != "/tmp" {
		t.Errorf("Dir = %q", cmd.Dir)
	}
	if cmd.Stdin != nil {
		t.Error("stdin must not be inherited by third-party code")
	}
	if cmd.Stdout == nil {
		t.Error("stdout should be redirected, not left to inherit the parent's")
	}
	if !slices.Contains(cmd.Env, "JSINGO_FD=3") {
		t.Errorf("JSINGO_FD missing from %v", cmd.Env)
	}
}

func TestEnvironIsSorted(t *testing.T) {
	t.Parallel()

	env := Policy{}.Environ(nil)
	if !slices.IsSorted(env) {
		t.Error("environment should be sorted for stable process listings")
	}
}

func TestHardenArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want []string
	}{
		{"node", []string{"--disallow-code-generation-from-strings"}},
		{"bun", []string{"--smol"}},
		{"deno", []string{"--deny-net", "--deny-env", "--deny-run", "--deny-ffi"}},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			got := HardenArgs(tc.kind, 0)
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("missing %q in %v", want, got)
				}
			}
		})
	}

	if got := HardenArgs("node", 512); !slices.Contains(got, "--max-old-space-size=512") {
		t.Errorf("heap cap missing from %v", got)
	}
	if got := HardenArgs("unknown", 0); len(got) != 0 {
		t.Errorf("unknown runtime should get no flags, got %v", got)
	}
}
