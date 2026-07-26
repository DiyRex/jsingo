package wire

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain fails the package if goroutines outlive the tests.
//
// The mux owns a long-lived read loop per connection, so a bug in the shutdown
// path leaks one goroutine per sidecar - invisible in a passing test suite and
// fatal in a long-running server. go.uber.org/goleak does this more
// thoroughly, but this module has no dependencies and that is worth keeping;
// the check below covers the only leak shape this package can produce.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if err := checkLeaks(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

func checkLeaks() error {
	// Goroutines wind down asynchronously after the last test returns, so
	// retry before declaring a leak.
	deadline := time.Now().Add(2 * time.Second)
	var stacks string
	for time.Now().Before(deadline) {
		stacks = leakedStacks()
		if stacks == "" {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("goroutine leak detected:\n%s", stacks)
}

// leakedStacks returns the stacks of goroutines that are neither runtime
// internals nor the test harness itself.
func leakedStacks() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	var leaked []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if g = strings.TrimSpace(g); g == "" || isExpectedGoroutine(g) {
			continue
		}
		leaked = append(leaked, g)
	}
	return strings.Join(leaked, "\n\n")
}

func isExpectedGoroutine(stack string) bool {
	// The reporting goroutine itself, plus runtime and testing machinery that
	// is always present at exit.
	expected := []string{
		"wire.leakedStacks",
		"runtime.goexit", // parked, no user frames
		"testing.(*M).Run",
		"testing.runTests",
		"runtime.gopark",
		"os/signal.signal_recv",
		"created by runtime",
	}
	// A goroutine is only interesting if it has frames from this package.
	if !strings.Contains(stack, "jsingo/internal/wire") {
		return true
	}
	for _, e := range expected {
		if strings.Contains(stack, e) && !strings.Contains(stack, "wire.(*Mux)") {
			return true
		}
	}
	return false
}
