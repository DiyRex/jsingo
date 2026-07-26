//go:build unix && !linux

package supervisor

import "syscall"

// setParentDeathSignal is a no-op outside Linux.
//
// macOS, the BSDs and Darwin have no equivalent of prctl(PR_SET_PDEATHSIG).
// If Go is killed with SIGKILL it runs no cleanup, so nothing in the parent
// can reap the sidecar. The JS host's heartbeat watchdog is the only remaining
// backstop there: it exits on its own once pings stop arriving. That is why
// the watchdog is mandatory rather than an optimisation.
func setParentDeathSignal(*syscall.SysProcAttr) {}

// HasParentDeathSignal reports whether the OS can kill orphaned children
// automatically. When false, the heartbeat watchdog is the only backstop and
// must not be disabled.
const HasParentDeathSignal = false
