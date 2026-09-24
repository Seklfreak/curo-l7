package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func testRecord(t *testing.T) Record {
	t.Helper()
	msg, _, ok := parseFrame(unhex(t, "73 50 17 21 90 19 03 0e 08 1e 00 c3 00 6e 00 37 00 00 00 00 01 07 00 00 2f aa"))
	if !ok {
		t.Fatal("frame not parsed")
	}
	r, err := parseRecord(msg, time.FixedZone("EDT", -4*3600))
	if err != nil {
		t.Fatal(err)
	}
	r.Time = r.MeterTime.Add(12 * time.Hour)
	return r
}

func TestToLabTracker(t *testing.T) {
	r := testRecord(t)
	got := toLabTracker(r, "p1")
	if got.ExternalID != "curo-l7:2190190"+"30e081e00c3006e0037000000000107"+"0000" {
		t.Errorf("external id %q", got.ExternalID)
	}
	// The id must not depend on the clock correction.
	r2 := r
	r2.Time = r.MeterTime
	if externalID(r2) != got.ExternalID {
		t.Error("external id changed with the clock offset")
	}
	if got.TakenAt != "2025-03-14T20:30:00-04:00" || got.Device != "CURO L7" || got.ProfileID != "p1" {
		t.Errorf("reading %+v", got)
	}
	want := []ltResult{{"Total Cholesterol", 195, "mg/dL"}, {"Triglycerides", 110, "mg/dL"}, {"HDL Cholesterol", 55, "mg/dL"}}
	if len(got.Results) != 3 {
		t.Fatalf("results %+v", got.Results)
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got.Results[i], want[i])
		}
	}
}

func TestAskProfile(t *testing.T) {
	profiles := []ltProfile{{"a", "Alex"}, {"b", "Sam"}}
	r := testRecord(t)
	cases := []struct {
		input string
		last  int
		want  int
		quit  bool
	}{
		{"2\n", -1, 1, false},
		{"\n", 0, 0, false},        // Enter repeats the last choice
		{"x\n\n1\n", -1, 0, false}, // invalid input and Enter without a default re-prompt
		{"s\n", 0, -1, false},
		{"q\n", 0, 0, true},
		{"", 0, 0, true}, // EOF quits
	}
	for _, c := range cases {
		got, err := askProfile(io.Discard, bufio.NewReader(strings.NewReader(c.input)), r, profiles, c.last)
		if c.quit {
			if err != errQuit {
				t.Errorf("%q: want quit, got %d, %v", c.input, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %d, %v; want %d", c.input, got, err, c.want)
		}
	}
}
