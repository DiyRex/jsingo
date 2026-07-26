//go:build linux

package supervisor

import "syscall"

// setParentDeathSignal asks the kernel to SIGKILL the child when its parent
// dies, covering the case where Go is killed with SIGKILL and never runs its
// own cleanup.
//
// This is the strongest guarantee available and it is Linux-only. Other unices
// rely on the heartbeat watchdog in the JS host; see pdeathsig_other.go.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}

// HasParentDeathSignal reports whether the OS can kill orphaned children
// automatically. When false, the heartbeat watchdog is the only backstop and
// must not be disabled.
const HasParentDeathSignal = true
