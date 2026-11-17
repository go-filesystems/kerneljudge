package kerneljudge

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// RegimeText is the regime of a series as printed in the PERF line: its
// median calibration figure, whether every round shared it, and the rounds'
// labels. ParsePerf reads it back.
func RegimeText(rs Regimes) string {
	state := "uniform"
	if !rs.Uniform() {
		state = "MIXED"
	}
	return fmt.Sprintf("regime %.0f Mops/s %s %v", rs.Median(), state, rs.Labels())
}

// PerfLine is one parsed "PERF" line of a --perf run.
type PerfLine struct {
	Driver, Op, Impl string
	Median           float64
	Unit             string
	// Regime is the median calibration figure (Mops/s) and Mixed is true when
	// the rounds did not all share it. Known is false for a line written
	// before regimes were recorded.
	Regime float64
	Mixed  bool
	Known  bool
}

// Key identifies the measurement a line is a sample of.
func (p PerfLine) Key() string { return p.Driver + " | " + p.Op + " | " + p.Impl }

var regimeRe = regexp.MustCompile(`regime (\d+) Mops/s (uniform|MIXED)`)

// ParsePerf reads the PERF lines of a kerneljudge log.
func ParsePerf(r io.Reader) ([]PerfLine, error) {
	var out []PerfLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		_, line, ok := strings.Cut(sc.Text(), "PERF ")
		if !ok {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 4 {
			continue
		}
		med := strings.Fields(f[3]) // "median", value, unit
		if len(med) < 3 || med[0] != "median" {
			continue
		}
		v, err := strconv.ParseFloat(med[1], 64)
		if err != nil {
			continue
		}
		p := PerfLine{Driver: strings.TrimSpace(f[0]), Op: strings.TrimSpace(f[1]), Impl: strings.TrimSpace(f[2]), Median: v, Unit: med[2]}
		if m := regimeRe.FindStringSubmatch(line); m != nil {
			p.Regime, _ = strconv.ParseFloat(m[1], 64)
			p.Mixed = m[2] == "MIXED"
			p.Known = true
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// ComparePerf compares two runs measurement by measurement and returns a
// report. It returns an error, after the report, when any compared pair does
// not share one regime: a line with no recorded regime, a series whose rounds
// were mixed, or two series whose calibrations differ by more than tol.
// allowMixed turns the error into a "NOT COMPARABLE" flag on the line.
func ComparePerf(a, b []PerfLine, tol float64, allowMixed bool) (string, error) {
	idx := map[string]PerfLine{}
	for _, p := range a {
		if _, dup := idx[p.Key()]; dup {
			return "", fmt.Errorf("%q appears twice in the first run: split the log", p.Key())
		}
		idx[p.Key()] = p
	}
	var sb strings.Builder
	bad := 0
	n := 0
	seen := map[string]bool{}
	for _, q := range b {
		if seen[q.Key()] {
			return sb.String(), fmt.Errorf("%q appears twice in the second run: split the log", q.Key())
		}
		seen[q.Key()] = true
		p, ok := idx[q.Key()]
		if !ok {
			fmt.Fprintf(&sb, "%-55s only in the second run\n", q.Key())
			bad++
			continue
		}
		n++
		why := ""
		switch {
		case !p.Known || !q.Known:
			why = "a regime was not recorded"
		case p.Mixed || q.Mixed:
			why = "the rounds of a run ran in different regimes"
		case !SameRegime(p.Regime, q.Regime, tol):
			why = fmt.Sprintf("regimes differ (%.0f vs %.0f Mops/s)", p.Regime, q.Regime)
		}
		flag := fmt.Sprintf("regime %.0f", q.Regime)
		if why != "" {
			bad++
			flag = "NOT COMPARABLE: " + why
		}
		fmt.Fprintf(&sb, "%-55s %9.1f -> %9.1f %s (%+.1f %%)  %s\n", q.Key(), p.Median, q.Median, q.Unit, (q.Median/p.Median-1)*100, flag)
	}
	for k := range idx {
		if !seen[k] {
			fmt.Fprintf(&sb, "%-55s only in the first run\n", k)
			bad++
		}
	}
	if n == 0 {
		return sb.String(), fmt.Errorf("no measurement is in both runs")
	}
	if bad > 0 && !allowMixed {
		return sb.String(), fmt.Errorf("%d of %d measurements ran in different regimes: not comparable (--allow-mixed to print them anyway)", bad, n)
	}
	return sb.String(), nil
}
