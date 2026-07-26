//go:build unix

package supervisor

import (
	"os/exec"
	"syscall"
)

// configureProcAttr puts the child in its own process group.
//
// Signalling the group rather than the process is what reaches grandchildren.
// A JS runtime that spawns a worker leaves it orphaned and running if only the
// direct child is killed, and that orphan keeps holding memory and file
// descriptors. Killing the group collects the whole tree.
func configureProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setParentDeathSignal(cmd.SysProcAttr)
}

// signalGroup sends sig to the child's whole process group.
//
// A negative pid addresses the group, which only works because
// configureProcAttr set Setpgid, making the child's pid its own group id.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
