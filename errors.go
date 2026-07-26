package jsingo

import (
	"errors"
	"fmt"

	"github.com/DiyRex/jsingo/internal/supervisor"
	"github.com/DiyRex/jsingo/internal/wire"
)

// Sentinel errors. Every failure from this package matches one of these with
// errors.Is, so callers branch on behaviour instead of parsing messages.
var (
	// ErrNotStarted means the Runtime has not finished starting. Calls made
	// before Start completes return this.
	ErrNotStarted = errors.New("jsingo: runtime not started")

	// ErrClosed means the Runtime has been closed.
	ErrClosed = errors.New("jsingo: runtime closed")

	// ErrSidecarRestarting means the sidecar died and is being respawned.
	//
	// This is retryable: wait and try again, or use WithAutoRetry to have
	// idempotent calls retried automatically.
	ErrSidecarRestarting = errors.New("jsingo: sidecar restarting")

	// ErrSidecarUnrecoverable means the sidecar failed too many times and will
	// not be restarted again. Retrying will not help; the process needs
	// operator attention.
	ErrSidecarUnrecoverable = errors.New("jsingo: sidecar unrecoverable")

	// ErrNoRuntime means no usable JavaScript runtime was found.
	ErrNoRuntime = errors.New("jsingo: no JavaScript runtime found")

	// ErrBundleMismatch means the extracted host bundle did not match its
	// expected hash, which means it was modified after the binary was built.
	ErrBundleMismatch = errors.New("jsingo: host bundle hash mismatch")
)

// Code classifies a failure raised by a JavaScript handler.
type Code uint16

// Codes mirror the subset of gRPC codes that carry meaning here, so a future
// gRPC transport maps across without translation loss.
const (
	CodeCanceled         = Code(wire.CodeCanceled)
	CodeUnknown          = Code(wire.CodeUnknown)
	CodeInvalidArgument  = Code(wire.CodeInvalidArgument)
	CodeDeadlineExceeded = Code(wire.CodeDeadlineExceed)
	CodeNotFound         = Code(wire.CodeNotFound)
	CodeUnimplemented    = Code(wire.CodeUnimplemented)
	CodeInternal         = Code(wire.CodeInternal)
	CodeUnavailable      = Code(wire.CodeUnavailable)
)

// String implements fmt.Stringer.
func (c Code) String() string { return wire.ErrorCode(c).String() }

// HandlerError is a failure raised inside a JavaScript handler.
//
// Match on the code alone with errors.Is:
//
//	if errors.Is(err, &jsingo.HandlerError{Code: jsingo.CodeNotFound}) { ... }
//
// or inspect the whole thing with errors.As.
type HandlerError struct {
	// Method is the handler that failed.
	Method string
	// Code classifies the failure. A handler that throws a plain Error
	// produces CodeInternal.
	Code Code
	// Message is the JavaScript error's message.
	Message string
	// Stack is the JavaScript stack trace, if the runtime produced one. This
	// is usually the only way to locate a fault inside a minified bundle.
	Stack string
}

func (e *HandlerError) Error() string {
	if e.Method == "" {
		return fmt.Sprintf("jsingo: %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("jsingo: %s: %s: %s", e.Method, e.Code, e.Message)
}

// Is matches on code alone when the target carries no message, so callers can
// write errors.Is(err, &HandlerError{Code: CodeNotFound}).
func (e *HandlerError) Is(target error) bool {
	t, ok := target.(*HandlerError)
	if !ok {
		return false
	}
	if t.Message != "" && t.Message != e.Message {
		return false
	}
	if t.Method != "" && t.Method != e.Method {
		return false
	}
	return t.Code == e.Code
}

// Retryable reports whether retrying the same call could plausibly succeed.
//
// It does not consider idempotency: a retryable failure of a non-idempotent
// operation may still be unsafe to repeat. That judgement belongs to the
// caller, which is why WithAutoRetry is opt-in per binding.
func Retryable(err error) bool {
	if errors.Is(err, ErrSidecarRestarting) || errors.Is(err, ErrNotStarted) {
		return true
	}
	var he *HandlerError
	if errors.As(err, &he) {
		return he.Code == CodeUnavailable
	}
	return false
}

// translate converts an internal error into the public vocabulary.
//
// Internal packages are free to change their errors; this is the one place
// that has to keep up, so the public sentinels stay stable.
func translate(method string, err error) error {
	if err == nil {
		return nil
	}

	var ce *wire.CallError
	if errors.As(err, &ce) {
		return &HandlerError{
			Method:  method,
			Code:    Code(ce.Code),
			Message: ce.Message,
			Stack:   string(ce.Details),
		}
	}

	switch {
	case errors.Is(err, supervisor.ErrCrashLoop):
		return fmt.Errorf("%w: %w", ErrSidecarUnrecoverable, err)
	case errors.Is(err, wire.ErrClosed):
		// The mux closes both when the sidecar dies and when we shut down.
		// The Runtime disambiguates before calling this; reaching here means
		// the sidecar went away underneath a call.
		return fmt.Errorf("%w: %w", ErrSidecarRestarting, err)
	case errors.Is(err, wire.ErrProtocol):
		return fmt.Errorf("jsingo: protocol violation by sidecar: %w", err)
	default:
		return err
	}
}
