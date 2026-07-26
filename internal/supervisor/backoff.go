package supervisor

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff computes restart delays with exponential growth and full jitter.
//
// Full jitter - a uniform pick from [0, cap] rather than cap/2 plus noise - is
// what stops a fleet of processes that failed together from retrying in
// lockstep and re-creating the load that killed them.
type Backoff struct {
	// Min is the delay after the first failure. Zero selects DefaultMinDelay.
	Min time.Duration
	// Max caps the delay. Zero selects DefaultMaxDelay.
	Max time.Duration
	// Factor multiplies the ceiling per attempt. Zero or less selects 2.
	Factor float64
	// rand allows tests to make jitter deterministic. Nil uses math/rand/v2.
	rand func() float64
}

// Backoff defaults.
const (
	DefaultMinDelay = 100 * time.Millisecond
	DefaultMaxDelay = 5 * time.Second
)

// Delay returns how long to wait before attempt n, counting from zero. The
// result is always in [0, Max].
func (b Backoff) Delay(n int) time.Duration {
	minD, maxD, factor := b.Min, b.Max, b.Factor
	if minD <= 0 {
		minD = DefaultMinDelay
	}
	if maxD <= 0 {
		maxD = DefaultMaxDelay
	}
	if factor <= 0 {
		factor = 2
	}
	if maxD < minD {
		maxD = minD
	}
	if n < 0 {
		n = 0
	}

	// Compute the ceiling in float64 and clamp before converting: a large n
	// would otherwise overflow int64 and produce a negative duration.
	ceiling := float64(minD) * math.Pow(factor, float64(n))
	if math.IsInf(ceiling, 0) || ceiling > float64(maxD) {
		ceiling = float64(maxD)
	}

	r := b.rand
	if r == nil {
		r = rand.Float64
	}
	return time.Duration(r() * ceiling)
}
