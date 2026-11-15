package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLoad(t *testing.T) {
	for in, want := range map[string]float64{"{ 7.34 9.73 12.75 }\n": 7.34, "{ 0.10 0.20 0.30 }": 0.10} {
		got, err := parseLoad(in)
		if err != nil || got != want {
			t.Errorf("parseLoad(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseLoad("{ }"); err == nil {
		t.Error("an empty load average parsed")
	}
	if _, err := parseLoad("{ x y z }"); err == nil {
		t.Error("a non-number parsed")
	}
}

func TestVerdict(t *testing.T) {
	pass := "=== RUN x\n--- PASS: TestKernelJudge (1s)\n--- SKIP: TestKernelJudgePerf (0s)\nPASS\n"
	if err := verdict(pass, false); err != nil {
		t.Errorf("a pass with the perf test skipped, no perf asked: %v", err)
	}
	if verdict(pass, true) == nil {
		t.Error("a skipped performance run counted as measured")
	}
	if verdict("testing: warning: no tests to run\nPASS\n", false) == nil {
		t.Error("a run of nothing counted as a pass")
	}
}

func TestRunNeedsADriver(t *testing.T) {
	var out bytes.Buffer
	cmd := RootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"run"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "driver") {
		t.Fatalf("run without --driver: %v", err)
	}
}
