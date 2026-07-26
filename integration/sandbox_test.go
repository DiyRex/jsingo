//go:build integration && unix

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are adversarial: they run the things a compromised npm
// dependency does first and assert the sandbox stops them. A control that is
// never tested against an actual attempt is a comment, not a control.

const canaryVar = "JSINGO_TEST_CANARY_SECRET"
const canaryValue = "sk_live_this_must_never_reach_the_sidecar"

// The single highest-value control: process.env must not contain the parent's
// secrets. Reading the environment is the cheapest thing a malicious package
// can do and the most valuable thing it can find.
func TestSidecarCannotReadParentSecrets(t *testing.T) {
	t.Setenv(canaryVar, canaryValue)

	eachRuntime(t, func(t *testing.T, s *session) {
		got, err := call[struct {
			Keys   []string          `json:"keys"`
			Values map[string]string `json:"values"`
		}](t, s, t.Context(), "dumpEnv", nil)
		if err != nil {
			t.Fatalf("dumpEnv: %v", err)
		}

		if v, ok := got.Values[canaryVar]; ok {
			t.Fatalf("the sidecar can read %s = %q", canaryVar, v)
		}
		for k, v := range got.Values {
			if strings.Contains(v, canaryValue) {
				t.Fatalf("canary leaked through %s = %q", k, v)
			}
		}

		// The environment should be small enough to audit by eye. A large one
		// means the parent's environment is being forwarded wholesale.
		if len(got.Keys) > 20 {
			t.Errorf("sidecar sees %d environment variables, expected a minimal set: %v",
				len(got.Keys), got.Keys)
		}
		t.Logf("sidecar environment: %v", got.Keys)
	})
}

// $HOME must not point at the real home directory, which is where
// ~/.aws/credentials, ~/.ssh/id_rsa, ~/.npmrc and ~/.kube/config live.
func TestSidecarHomeIsNotTheRealHome(t *testing.T) {
	t.Parallel()

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	eachRuntime(t, func(t *testing.T, s *session) {
		got, err := call[struct {
			Values map[string]string `json:"values"`
		}](t, s, t.Context(), "dumpEnv", nil)
		if err != nil {
			t.Fatalf("dumpEnv: %v", err)
		}

		if home := got.Values["HOME"]; home == realHome {
			t.Fatalf("sidecar HOME is the real home %q; credential files are reachable", home)
		} else {
			t.Logf("sidecar HOME = %q", home)
		}
	})
}

// Demonstrates the filesystem reach that remains, so the boundary is recorded
// rather than assumed. Confinement here is the container's job - see
// docs/SECURITY.md - and this test documents precisely why.
func TestSidecarFilesystemReachIsDocumented(t *testing.T) {
	t.Parallel()

	secret := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(secret, []byte("[default]\naws_secret_access_key=AKIA..."), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	eachRuntime(t, func(t *testing.T, s *session) {
		got, err := call[struct {
			OK  bool   `json:"ok"`
			Err string `json:"err"`
		}](t, s, t.Context(), "readPath", map[string]string{"path": secret})
		if err != nil {
			t.Fatalf("readPath: %v", err)
		}

		if got.OK {
			t.Logf("EXPECTED on a bare host: the sidecar read %s. "+
				"Process-level controls cannot prevent this; a read-only rootfs and "+
				"a dedicated uid in the container are what confine it.", secret)
		} else {
			t.Logf("filesystem read denied: %s", got.Err)
		}
	})
}

// Same for network egress: unconfined on a bare host, which is exactly why the
// deployment manifests set a default-deny NetworkPolicy.
func TestSidecarNetworkReachIsDocumented(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		// The cloud metadata endpoint - the first thing an exfiltration
		// payload tries, because it yields live IAM credentials.
		got, err := call[struct {
			OK  bool   `json:"ok"`
			Err string `json:"err"`
		}](t, s, t.Context(), "connectOut", map[string]any{"host": "169.254.169.254", "port": 80})
		if err != nil {
			t.Fatalf("connectOut: %v", err)
		}

		if got.OK {
			t.Errorf("the sidecar reached the cloud metadata endpoint; " +
				"block 169.254.169.254 with a NetworkPolicy before deploying")
		} else {
			t.Logf("metadata endpoint unreachable: %s", got.Err)
		}
	})
}

// node is given --disallow-code-generation-from-strings, which removes eval
// and new Function. A large share of obfuscated npm payloads stage through
// one of the two, so this records whether the flag is actually in effect.
func TestCodeGenerationPolicy(t *testing.T) {
	t.Parallel()

	eachRuntime(t, func(t *testing.T, s *session) {
		got, err := call[struct {
			OK  bool   `json:"ok"`
			Err string `json:"err"`
		}](t, s, t.Context(), "canEval", nil)
		if err != nil {
			t.Fatalf("canEval: %v", err)
		}
		t.Logf("dynamic code generation available: %v (%s)", got.OK, got.Err)
	})
}
