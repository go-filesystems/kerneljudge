package kerneljudge

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// calibrateOps is the fixed amount of work every vCPU does in one calibration
// slice: a dependent chain of shifts and xors, so the figure is the core's
// clock times its issue width and nothing else (no memory, no kernel).
const calibrateOps = 100_000_000

//go:noinline
func spin(n uint64) uint64 {
	x := uint64(88172645463325252)
	for i := uint64(0); i < n; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	return x
}

// Calibration is one reading of the machine's speed: the fixed loop run on
// Workers threads at once, repeated, the median kept.
type Calibration struct {
	Workers int
	// Mops is the aggregate speed over all workers, in millions of loop
	// iterations per second (the median of the repeats).
	Mops float64
	// Min and Max are the slowest and fastest single worker of that repeat: a
	// wide gap is a vCPU on a different kind of core; a uniformly low figure is
	// every vCPU time-sliced by a busy host.
	Min, Max float64
}

func (c Calibration) String() string {
	return fmt.Sprintf("%.0f Mops/s on %d vCPU (%.0f-%.0f each)", c.Mops, c.Workers, c.Min, c.Max)
}

// Calibrate times the fixed loop on workers threads (0 = every CPU) three
// times and returns the median reading. It takes about half a second.
func Calibrate(workers int) Calibration {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	return calibrateWith(workers, calibrateOps, 3, func(n uint64) { spin(n) })
}

// calibrateWith is Calibrate with the work function replaceable, so a test can
// give it a slow or a fast machine.
func calibrateWith(workers int, ops uint64, repeats int, work func(uint64)) Calibration {
	prev := runtime.GOMAXPROCS(workers)
	defer runtime.GOMAXPROCS(prev)
	reads := make([]Calibration, repeats)
	for r := range reads {
		per := make([]float64, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				st := time.Now()
				work(ops)
				per[w] = float64(ops) / time.Since(st).Seconds() / 1e6
			}()
		}
		wg.Wait()
		c := Calibration{Workers: workers, Min: per[0], Max: per[0]}
		for _, v := range per {
			c.Mops += v
			c.Min = min(c.Min, v)
			c.Max = max(c.Max, v)
		}
		reads[r] = c
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i].Mops < reads[j].Mops })
	return reads[len(reads)/2]
}

// DefaultRegimeTolerance is how far two calibrations may differ (relative to
// the larger) and still be the same regime. Measured on fsjudge-vm at 4 CPUs:
// an idle host repeats within 5 %, a busy one reads a third of that.
const DefaultRegimeTolerance = 0.10

// SameRegime reports whether two calibration readings (Mops/s) are within tol
// of each other, relative to the larger.
func SameRegime(a, b, tol float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	hi, lo := max(a, b), min(a, b)
	return (hi-lo)/hi <= tol
}

// Regime is the machine's speed class during one measurement round: the
// calibration taken before the round and the one taken after it.
type Regime struct {
	Before, After float64 // Mops/s
	Tol           float64
}

// Steady reports whether the machine kept one speed across the round.
func (g Regime) Steady() bool { return SameRegime(g.Before, g.After, g.Tol) }

// Mops is the round's figure: the lower of the two readings, so a round that
// straddles a change reads as the slow side.
func (g Regime) Mops() float64 { return min(g.Before, g.After) }

// Label is the regime as printed next to a measurement: the calibration figure
// rounded to 50 Mops/s (so that readings of one regime print alike), with a
// "~" when the machine changed speed during the round.
func (g Regime) Label() string {
	l := fmt.Sprintf("%d", int(g.Mops()/50+0.5)*50)
	if !g.Steady() {
		l += "~"
	}
	return l
}

// settleRound runs one measurement round between two calibrations. A round
// whose calibrations differ, or that ran in another regime than first (nil for
// the first round), is undone with discard and run again, up to maxRetries
// times; the last attempt is returned whatever its regime, so the caller
// records and flags it.
func settleRound(calib func() float64, run func(), discard func(Regime), first *Regime, tol float64, maxRetries int) Regime {
	for attempt := 0; ; attempt++ {
		g := Regime{Before: calib(), Tol: tol}
		run()
		g.After = calib()
		ref := g
		if first != nil {
			ref = *first
		}
		if (g.Steady() && SameRegime(g.Mops(), ref.Mops(), tol)) || attempt >= maxRetries {
			return g
		}
		discard(g)
	}
}

// Regimes is the list of the rounds' regimes of one measurement series.
type Regimes []Regime

// Uniform reports whether every round was steady and ran in the regime of the
// first.
func (rs Regimes) Uniform() bool {
	for _, g := range rs {
		if !g.Steady() || !SameRegime(g.Mops(), rs[0].Mops(), g.Tol) {
			return false
		}
	}
	return len(rs) > 0
}

// Median is the median round figure (Mops/s): the series' regime.
func (rs Regimes) Median() float64 {
	s := make([]float64, len(rs))
	for i, g := range rs {
		s[i] = g.Mops()
	}
	sort.Float64s(s)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}

// Labels lists the rounds' labels.
func (rs Regimes) Labels() []string {
	l := make([]string, len(rs))
	for i, g := range rs {
		l[i] = g.Label()
	}
	return l
}
