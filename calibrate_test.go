package kerneljudge

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSameRegimeBoundary(t *testing.T) {
	for _, c := range []struct {
		a, b float64
		want bool
	}{
		{2550, 2550, true},
		{2550, 2400, true},  // 5.9 %: one regime
		{2500, 2250, true},  // exactly 10 %
		{2500, 2249, false}, // just over
		{2549, 800, false},  // the slow regime seen on 2026-11-17
		{0, 2500, false},
		{2500, -1, false},
	} {
		if got := SameRegime(c.a, c.b, 0.10); got != c.want {
			t.Errorf("SameRegime(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := SameRegime(c.b, c.a, 0.10); got != c.want {
			t.Errorf("SameRegime(%v,%v) not symmetric", c.b, c.a)
		}
	}
}

func TestCalibrateReadsASlowMachineAsSlow(t *testing.T) {
	fast := calibrateWith(2, 1_000_000, 3, func(n uint64) { spin(n) })
	slow := calibrateWith(2, 1_000_000, 3, func(n uint64) { spin(n); time.Sleep(20 * time.Millisecond) })
	if fast.Workers != 2 || fast.Mops <= 0 || fast.Min > fast.Max {
		t.Fatalf("bad reading %+v", fast)
	}
	if SameRegime(fast.Mops, slow.Mops, DefaultRegimeTolerance) {
		t.Errorf("a machine delayed by 20 ms per slice read as the same regime: %v vs %v", fast, slow)
	}
	if !strings.Contains(fast.String(), "Mops/s on 2 vCPU") {
		t.Errorf("String: %q", fast)
	}
}

func TestRegimeSteadyAndLabel(t *testing.T) {
	steady := Regime{Before: 2560, After: 2540, Tol: 0.1}
	if !steady.Steady() || steady.Label() != "2550" {
		t.Errorf("steady: %v %q", steady.Steady(), steady.Label())
	}
	straddle := Regime{Before: 2560, After: 900, Tol: 0.1}
	if straddle.Steady() || straddle.Mops() != 900 || straddle.Label() != "900~" {
		t.Errorf("straddle: %v %v %q", straddle.Steady(), straddle.Mops(), straddle.Label())
	}
}

func TestRegimesUniform(t *testing.T) {
	a := Regime{Before: 2550, After: 2560, Tol: 0.1}
	b := Regime{Before: 2500, After: 2520, Tol: 0.1}
	slow := Regime{Before: 900, After: 910, Tol: 0.1}
	straddle := Regime{Before: 2550, After: 900, Tol: 0.1}
	if !(Regimes{a, b, a}).Uniform() {
		t.Error("three rounds of one regime are not uniform")
	}
	if (Regimes{a, slow}).Uniform() {
		t.Error("a slow round after a fast one is uniform")
	}
	if (Regimes{slow, a}).Uniform() {
		t.Error("a fast round after a slow one is uniform")
	}
	if (Regimes{a, straddle}).Uniform() {
		t.Error("a straddling round is uniform")
	}
	if (Regimes{}).Uniform() {
		t.Error("no rounds is uniform")
	}
	if got := RegimeText(Regimes{a, slow, b}); !strings.Contains(got, "MIXED") || !strings.Contains(got, "[2550 900 2500]") {
		t.Errorf("RegimeText = %q", got)
	}
	if got := RegimeText(Regimes{a, b}); !strings.Contains(got, "uniform") || !strings.Contains(got, "regime 2550 Mops/s") {
		t.Errorf("RegimeText = %q", got)
	}
}

func perfLog(regime string, kernel, ours float64) string {
	return "    perf.go:1: PERF sq | seq read      | kernel | median " + f1(kernel) + " MiB/s | rounds 1 2 3 | | " + regime + "\n" +
		"    perf.go:1: PERF sq | seq read      | ours   | median " + f1(ours) + " MiB/s | rounds 1 2 3 | note | " + regime + "\n"
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func TestParseAndComparePerf(t *testing.T) {
	fast := "regime 2550 Mops/s uniform [2550 2550 2550]"
	slow := "regime 900 Mops/s uniform [900 900 900]"
	mixed := "regime 900 Mops/s MIXED [2550 900 900]"
	parse := func(s string) []PerfLine {
		p, err := ParsePerf(strings.NewReader(s))
		if err != nil || len(p) != 2 {
			t.Fatalf("ParsePerf: %v %d", err, len(p))
		}
		return p
	}
	a, same, other, mx := parse(perfLog(fast, 280, 760)), parse(perfLog(fast, 290, 770)), parse(perfLog(slow, 200, 570)), parse(perfLog(mixed, 200, 570))
	if !a[0].Known || a[0].Regime != 2550 || a[0].Mixed || a[1].Median != 760 || a[1].Impl != "ours" || a[0].Key() != "sq | seq read | kernel" {
		t.Fatalf("parsed %+v", a)
	}
	if rep, err := ComparePerf(a, same, 0.1, false); err != nil || !strings.Contains(rep, "+") {
		t.Errorf("same regime refused: %v\n%s", err, rep)
	}
	rep, err := ComparePerf(a, other, 0.1, false)
	if err == nil || !strings.Contains(rep, "NOT COMPARABLE: regimes differ (2550 vs 900") {
		t.Errorf("different regimes accepted: %v\n%s", err, rep)
	}
	if _, err := ComparePerf(a, other, 0.1, true); err != nil {
		t.Errorf("--allow-mixed still failed: %v", err)
	}
	if rep, err := ComparePerf(a, mx, 0.1, false); err == nil || !strings.Contains(rep, "rounds of a run ran in different regimes") {
		t.Errorf("a MIXED run accepted: %v\n%s", err, rep)
	}
	old, _ := ParsePerf(strings.NewReader("PERF sq | seq read | kernel | median 280.0 MiB/s | rounds 1 2 | \n"))
	if old[0].Known {
		t.Error("a line with no regime reads as known")
	}
	if _, err := ComparePerf(old, a[:1], 0.1, false); err == nil {
		t.Error("a line with no regime was compared")
	}
	if _, err := ComparePerf(a[:1], parse(perfLog(fast, 1, 1))[1:], 0.1, false); err == nil {
		t.Error("two runs with no measurement in common compared clean")
	}
}

func TestSettleRound(t *testing.T) {
	seq := func(v ...float64) func() float64 {
		i := 0
		return func() float64 { x := v[i]; i++; return x }
	}
	fast := &Regime{Before: 2550, After: 2550, Tol: 0.1}
	runs, discards := 0, 0
	// before, after of attempt 1 straddle; attempt 2 is slow; attempt 3 is fast.
	g := settleRound(seq(2550, 900, 900, 910, 2540, 2560), func() { runs++ }, func(Regime) { discards++ }, fast, 0.1, 2)
	if runs != 3 || discards != 2 || g.Label() != "2550" || !g.Steady() {
		t.Errorf("runs %d discards %d regime %v", runs, discards, g)
	}
	// retries exhausted: the slow round is kept, and reads as slow.
	runs, discards = 0, 0
	g = settleRound(seq(900, 900, 900, 900, 900, 900), func() { runs++ }, func(Regime) { discards++ }, fast, 0.1, 2)
	if runs != 3 || discards != 2 || SameRegime(g.Mops(), fast.Mops(), 0.1) {
		t.Errorf("exhausted: runs %d discards %d %v", runs, discards, g)
	}
	// the first round has no reference: a steady round is accepted at once, whatever its speed.
	runs = 0
	g = settleRound(seq(900, 910), func() { runs++ }, func(Regime) { t.Error("discarded") }, nil, 0.1, 2)
	if runs != 1 || !g.Steady() {
		t.Errorf("first round: %d %v", runs, g)
	}
	// 0 retries never discards.
	runs = 0
	settleRound(seq(2550, 900), func() { runs++ }, func(Regime) { t.Error("discarded") }, nil, 0.1, 0)
	if runs != 1 {
		t.Errorf("no retries: %d", runs)
	}
}
