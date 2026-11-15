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
