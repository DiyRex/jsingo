//go:build unix

// Package transport creates the byte channel between Go and the sidecar.
//
// On unix that channel is an anonymous socketpair created before the child is
// forked. The child inherits one end as a numbered file descriptor; Go keeps
// the other.
//
// This is deliberately not a named Unix domain socket. A path-based socket
// would require allocating a unique path, creating and chmod-ing a private
// directory, polling for the file to appear before dialling, sweeping stale
// files left by killed processes, and checking peer credentials on accept to
// stop another local user connecting. A socketpair has no path, so none of
// those failure modes exist: there is nothing to collide with, nothing to go
// stale, and the descriptor is reachable only by processes we fork ourselves.
//
// It also removes the startup race outright. The connection exists before the
// process does, so there is no window in which Go can dial a socket the
// sidecar has not yet bound.
package transport

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// ChildFD is the descriptor number the sidecar sees.
//
// Descriptors 0, 1 and 2 are stdin, stdout and stderr; exec.Cmd.ExtraFiles
// starts at 3. The sidecar opens this number directly rather than being told
// a path.
const ChildFD = 3

// Pair is a connected pair of endpoints.
//
// Local is Go's end. Child must be passed to exec.Cmd.ExtraFiles and closed by
// the parent once the child has started: the parent holding it open would keep
// the connection alive after the child dies, so Go would never observe EOF.
type Pair struct {
	Local net.Conn
	Child *os.File
}

// NewPair creates a connected stream socketpair.
//
// Both descriptors are marked close-on-exec so an unrelated concurrent fork
// cannot inherit them; exec.Cmd clears that flag on the descriptors it passes
// through ExtraFiles, so the intended child still receives its end.
//
// SOCK_CLOEXEC cannot be requested in the socketpair type argument portably -
// Linux accepts it, macOS does not - so the flag is set afterwards under
// syscall.ForkLock. Holding the lock across creation and marking is what
// closes the window in which another goroutine's fork could inherit an
// unmarked descriptor. This mirrors what the standard library does in os.Pipe
// on platforms lacking the atomic form.
func NewPair() (*Pair, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()

	if err != nil {
		return nil, fmt.Errorf("transport: socketpair: %w", err)
	}

	// From here on either descriptor may need closing on an error path, so
	// wrap both in *os.File immediately and let the finaliser-free explicit
	// closes below do the work.
	localFile := os.NewFile(uintptr(fds[0]), "jsingo-local")
	childFile := os.NewFile(uintptr(fds[1]), "jsingo-child")
	if localFile == nil || childFile == nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
		return nil, fmt.Errorf("transport: socketpair returned an invalid descriptor")
	}

	// net.FileConn dups the descriptor and registers the copy with the
	// runtime poller, so reads and writes park a goroutine instead of blocking
	// a thread. The original is then redundant.
	local, err := net.FileConn(localFile)
	closeErr := localFile.Close()
	if err != nil {
		_ = childFile.Close()
		return nil, fmt.Errorf("transport: adopt local end: %w", err)
	}
	if closeErr != nil {
		_ = local.Close()
		_ = childFile.Close()
		return nil, fmt.Errorf("transport: close duplicated local end: %w", closeErr)
	}

	return &Pair{Local: local, Child: childFile}, nil
}

// CloseChild releases the parent's copy of the child's endpoint.
//
// Call it immediately after starting the child. Until it returns, the parent
// still holds the far end open, and a sidecar crash would not surface as EOF
// on Local - the supervisor would wait forever for a process that is already
// gone.
func (p *Pair) CloseChild() error {
	if p.Child == nil {
		return nil
	}
	err := p.Child.Close()
	p.Child = nil
	if err != nil {
		return fmt.Errorf("transport: close child endpoint: %w", err)
	}
	return nil
}

// Close releases both endpoints. It is safe to call more than once.
func (p *Pair) Close() error {
	var firstErr error
	if p.Child != nil {
		if err := p.CloseChild(); err != nil {
			firstErr = err
		}
	}
	if p.Local != nil {
		if err := p.Local.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("transport: close local endpoint: %w", err)
		}
		p.Local = nil
	}
	return firstErr
}
