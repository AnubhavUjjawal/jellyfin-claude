package main

import "testing"

func TestParseTime(t *testing.T) {
	cases := map[string]float64{"90": 90, "1:30": 90, "1:02:03": 3723, "-15": -15, "2.5": 2.5}
	for in, want := range cases {
		got, err := parseTime(in)
		if err != nil || got != want {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTime("soon"); err == nil {
		t.Error("parseTime(soon) should fail")
	}
}

func TestFmtDur(t *testing.T) {
	cases := map[float64]string{0: "0:00", 59.6: "1:00", 185: "3:05", 3723: "1:02:03"}
	for in, want := range cases {
		if got := fmtDur(in); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", in, got, want)
		}
	}
}
