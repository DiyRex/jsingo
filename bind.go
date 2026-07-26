package jsingo

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Call is a typed handle on one JavaScript function.
type Call[In, Out any] func(context.Context, In) (Out, error)

// BindOption configures a single binding.
type BindOption func(*bindConfig)

type bindConfig struct {
	idempotent bool
	maxRetries int
	timeout    time.Duration
	qualified  bool
}

// Idempotent marks the call safe to retry automatically.
//
// Retries are opt-in per binding rather than global because retrying is only
// safe when the caller says so. A charge, a send, an append: repeating those
// after a sidecar restart is worse than failing. Read-only transforms - parse,
// render, sanitise - are the intended users.
func Idempotent() BindOption {
	return func(c *bindConfig) {
		c.idempotent = true
		if c.maxRetries == 0 {
			c.maxRetries = 2
		}
	}
}

// MaxRetries bounds automatic retries. It has no effect without [Idempotent].
func MaxRetries(n int) BindOption {
	return func(c *bindConfig) { c.maxRetries = n }
}

// Timeout applies a per-call deadline when the caller's context has none.
//
// A caller's own deadline always wins if it is shorter.
func Timeout(d time.Duration) BindOption {
	return func(c *bindConfig) { c.timeout = d }
}

// Qualified addresses the export as "module:name" rather than by bare name.
//
// Needed when two modules export the same name, since the bare form is
// ambiguous then and is not registered.
func Qualified() BindOption {
	return func(c *bindConfig) { c.qualified = true }
}

// Bind returns a typed function that calls an exported JavaScript function.
//
//	parse := jsingo.Bind[ParseReq, ParseResp](rt, article, "parseArticle")
//	resp, err := parse(ctx, ParseReq{HTML: raw})
//
// In is encoded with the Runtime's codec and Out decoded from the reply. There
// is no schema, no generated code and no interface to implement: the Go types
// are the contract.
//
// A failure inside the handler comes back as [*HandlerError] carrying the code,
// message and JavaScript stack.
func Bind[In, Out any](rt *Runtime, mod *Mod, method string, opts ...BindOption) Call[In, Out] {
	cfg := &bindConfig{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	name := method
	if cfg.qualified && mod != nil {
		name = mod.Name() + ":" + method
	}

	// Configuration errors are captured once and returned from every call,
	// rather than panicking at package initialisation.
	var bindErr error
	switch {
	case rt == nil:
		bindErr = errors.New("jsingo: Bind: nil Runtime")
	case method == "":
		bindErr = errors.New("jsingo: Bind: empty method name")
	case mod != nil && mod.err != nil:
		bindErr = mod.err
	}

	return func(ctx context.Context, in In) (Out, error) {
		var zero Out
		if bindErr != nil {
			return zero, bindErr
		}

		if cfg.timeout > 0 {
			if _, hasDeadline := ctx.Deadline(); !hasDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
				defer cancel()
			}
		}

		body, err := rt.cfg.codec.Encode(in)
		if err != nil {
			return zero, fmt.Errorf("jsingo: %s: %w", name, err)
		}

		raw, err := rt.callWithRetry(ctx, name, body, cfg)
		if err != nil {
			return zero, err
		}

		var out Out
		if err := rt.cfg.codec.Decode(raw, &out); err != nil {
			return zero, fmt.Errorf("jsingo: %s: %w", name, err)
		}
		return out, nil
	}
}

// callWithRetry performs the call, retrying only when the binding permits it.
func (r *Runtime) callWithRetry(ctx context.Context, method string, body []byte, cfg *bindConfig) ([]byte, error) {
	attempts := 1
	if cfg.idempotent && cfg.maxRetries > 0 {
		attempts += cfg.maxRetries
	}

	var lastErr error
	for i := range attempts {
		out, err := r.call(ctx, method, body)
		if err == nil {
			return out, nil
		}
		lastErr = err

		// Only a transient failure is worth repeating, and never after the
		// caller has given up.
		if !Retryable(err) || ctx.Err() != nil || i == attempts-1 {
			break
		}

		// A short wait lets the supervisor finish respawning; without it the
		// retries burn through the budget while the sidecar is still starting.
		select {
		case <-time.After(retryDelay(i)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// retryDelay grows with the attempt number.
func retryDelay(attempt int) time.Duration {
	d := 50 * time.Millisecond << attempt
	if d > time.Second {
		d = time.Second
	}
	return d
}
