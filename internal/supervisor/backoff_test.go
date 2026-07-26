package supervisor

import (
	"testing"
	"time"
)

func TestBackoffStaysWithinBounds(t *testing.T) {
	t.Parallel()

	b := Backoff{Min: 100 * time.Millisecond, Max: 5 * time.Second}
	for n := range 40 {
		for range 20 {
			d := b.Delay(n)
			if d < 0 {
				t.Fatalf("attempt %d: negative delay %v", n, d)
			}
			if d > b.Max {
				t.Fatalf("attempt %d: %v exceeds max %v", n, d, b.Max)
			}
		}
	}
}

// The ceiling must grow but never overflow into a negative or absurd duration
// for a process that has been failing for a long time.
func TestBackoffCeilingGrowsThenSaturates(t *testing.T) {
	t.Parallel()

	// rand() == 1 yields exactly the ceiling, making growth observable.
	b := Backoff{Min: 10 * time.Millisecond, Max: time.Second, rand: func() float64 { return 1 }}

	prev := time.Duration(0)
	for n := range 8 {
		d := b.Delay(n)
		if d < prev {
			t.Fatalf("attempt %d: ceiling shrank from %v to %v", n, prev, d)
		}
		prev = d
	}
	if prev != b.Max {
		t.Fatalf("ceiling saturated at %v, want %v", prev, b.Max)
	}

	// Far past saturation, including values that would overflow int64 if the
	// exponent were applied before clamping.
	for _, n := range []int{63, 64, 1000, 1 << 20} {
		if d := b.Delay(n); d != b.Max {
			t.Fatalf("attempt %d: %v, want %v", n, d, b.Max)
		}
	}
}

// Full jitter means a uniform pick from [0, ceiling]. Without it, a fleet that
// failed together retries in lockstep and recreates the load that killed it.
func TestBackoffAppliesFullJitter(t *testing.T) {
	t.Parallel()

	b := Backoff{Min: time.Second, Max: time.Second}

	var lo, hi int
	const runs = 400
	for range runs {
		if b.Delay(5) < 500*time.Millisecond {
			lo++
		} else {
			hi++
		}
	}
	// A fixed or near-cap delay would land everything in one bucket.
	if lo == 0 || hi == 0 {
		t.Fatalf("delays not spread across the range: %d low, %d high", lo, hi)
	}
}

func TestBackoffZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var b Backoff
	for n := range 10 {
		d := b.Delay(n)
		if d < 0 || d > DefaultMaxDelay {
			t.Fatalf("attempt %d: %v outside [0, %v]", n, d, DefaultMaxDelay)
		}
	}
}

func TestBackoffHandlesDegenerateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		b    Backoff
	}{
		{"max below min", Backoff{Min: time.Second, Max: time.Millisecond}},
		{"negative factor", Backoff{Min: time.Second, Max: 2 * time.Second, Factor: -1}},
		{"negative min", Backoff{Min: -time.Second, Max: time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for n := range 5 {
				if d := tc.b.Delay(n); d < 0 {
					t.Fatalf("attempt %d: negative delay %v", n, d)
				}
			}
		})
	}
}

func TestBackoffNegativeAttemptIsTreatedAsFirst(t *testing.T) {
	t.Parallel()

	b := Backoff{Min: 10 * time.Millisecond, Max: time.Second, rand: func() float64 { return 1 }}
	if got, want := b.Delay(-5), b.Delay(0); got != want {
		t.Fatalf("Delay(-5) = %v, want %v", got, want)
	}
}
