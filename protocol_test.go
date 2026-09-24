package main

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildFrame(t *testing.T) {
	// Captured from a real session.
	if got, want := buildFrame(msgAck), unhex(t, "73 10 04 10 40 50 aa"); !bytes.Equal(got, want) {
		t.Errorf("ack frame = % x, want % x", got, want)
	}
	if got, want := buildFrame(msgNext), unhex(t, "73 10 04 10 60 70 aa"); !bytes.Equal(got, want) {
		t.Errorf("next frame = % x, want % x", got, want)
	}
}

func TestParseFrameSkipsPowerOnGarbage(t *testing.T) {
	buf := unhex(t, "00 73 50 04 10 30 20 aa 73 50")
	msg, n, ok := parseFrame(buf)
	if !ok || !bytes.Equal(msg, msgGreeting) || n != 8 {
		t.Fatalf("got msg=% x n=%d ok=%v", msg, n, ok)
	}
	// Remaining partial frame needs more data and must not be consumed.
	if _, n, ok := parseFrame(buf[n:]); ok || n != 0 {
		t.Fatalf("partial frame: n=%d ok=%v", n, ok)
	}
}

func TestParseCount(t *testing.T) {
	buf := unhex(t, "73 50 18 30 00 03 aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa aa 99 aa")
	msg, _, ok := parseFrame(buf)
	if !ok {
		t.Fatal("frame not parsed")
	}
	n, err := parseCount(msg)
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

func TestParseRecord(t *testing.T) {
	// Made-up values in the layout of real captured records. The last one has
	// TG >= 400, where Friedewald LDL is not valid (ldl 0).
	cases := []struct {
		frame       string
		meterTime   string
		tc, tg, hdl int
		ldl, nonHDL int
		flag        byte
	}{
		{"73 50 17 21 90 19 03 0e 08 1e 00 c3 00 6e 00 37 00 00 00 00 01 07 00 00 2f aa", "2025-03-14 08:30", 195, 110, 55, 118, 140, 0x07},
		{"73 50 17 21 90 19 03 0e 08 15 00 96 00 3e 00 30 00 00 00 00 01 07 00 00 26 aa", "2025-03-14 08:21", 150, 62, 48, 90, 102, 0x07},
		{"73 50 17 21 90 19 02 02 13 2d 00 e7 00 b4 00 27 00 00 00 00 01 1f 00 00 fc aa", "2025-02-02 19:45", 231, 180, 39, 156, 192, 0x1f},
		{"73 50 17 21 90 19 01 05 07 05 00 fa 01 a4 00 23 00 00 00 00 01 07 00 00 d4 aa", "2025-01-05 07:05", 250, 420, 35, 0, 215, 0x07},
	}
	for _, c := range cases {
		msg, _, ok := parseFrame(unhex(t, c.frame))
		if !ok {
			t.Fatalf("frame not parsed: %s", c.frame)
		}
		r, err := parseRecord(msg, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.MeterTime.Format(timeLayout); got != c.meterTime {
			t.Errorf("time = %s, want %s", got, c.meterTime)
		}
		ldl, _ := r.LDL()
		if r.TC != c.tc || r.TG != c.tg || r.HDL != c.hdl || ldl != c.ldl || r.NonHDL() != c.nonHDL || r.Flag != c.flag {
			t.Errorf("%s: got %+v ldl=%d", c.meterTime, r, ldl)
		}
	}
}
