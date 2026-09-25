package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHex(t *testing.T) {
	got, err := parseHex(" 21 90\t0a ff ")
	if err != nil || !bytes.Equal(got, []byte{0x21, 0x90, 0x0a, 0xff}) {
		t.Errorf("got % x, %v", got, err)
	}
	for _, bad := range []string{"zz", "100", "1 2 x"} {
		if _, err := parseHex(bad); err == nil {
			t.Errorf("parseHex(%q) should fail", bad)
		}
	}
}

func TestReadRecordsFile(t *testing.T) {
	recs, err := readRecordsFile(writeReadingsFile(t))
	if err != nil || len(recs) != 3 || recs[2].HDL != 39 {
		t.Fatalf("recs=%+v err=%v", recs, err)
	}

	dir := t.TempDir()
	for name, content := range map[string]string{
		"not-json.json":   "hello",
		"bad-hex.json":    `[{"raw":"21 90 zz"}]`,
		"bad-record.json": `[{"raw":"21 90 19"}]`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRecordsFile(p); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A file saved with -format json reads back to the same records.
func TestReadRecordsFile_RoundTrip(t *testing.T) {
	r := testRecord(t)
	var buf bytes.Buffer
	if err := writeJSON(&buf, []Record{r}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := readRecordsFile(p)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	// Meter times are wall-clock times, read back in the local zone.
	if recs[0].Raw != r.Raw || recs[0].TC != r.TC || recs[0].MeterTime.Format(timeLayout) != r.MeterTime.Format(timeLayout) {
		t.Errorf("round trip changed the record: %+v vs %+v", recs[0], r)
	}
}

func TestLoadConfig_Precedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LAB_TRACKER_URL", "")
	t.Setenv("LAB_TRACKER_TOKEN", "")
	dir := filepath.Join(home, ".config", "curo-l7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(importConfig{URL: "https://file.example", Token: "lt_file"})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil || cfg.URL != "https://file.example" || cfg.Token != "lt_file" {
		t.Errorf("file only: %+v, %v", cfg, err)
	}

	// Environment variables win, each on its own.
	t.Setenv("LAB_TRACKER_TOKEN", "lt_env")
	cfg, err = loadConfig()
	if err != nil || cfg.URL != "https://file.example" || cfg.Token != "lt_env" {
		t.Errorf("env token: %+v, %v", cfg, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Errorf("broken file: err = %v", err)
	}
}

// fixedRecord is testRecord pinned to UTC so the output is stable everywhere.
func fixedRecord(t *testing.T) Record {
	r := testRecord(t)
	r.MeterTime = r.MeterTime.In(time.UTC)
	r.Time = r.MeterTime.Add(12 * time.Hour)
	return r
}

func TestWriteTable(t *testing.T) {
	var buf bytes.Buffer
	if err := writeTable(&buf, []Record{fixedRecord(t)}); err != nil {
		t.Fatal(err)
	}
	want := "" +
		"              Time   TC   TG  HDL  LDL*  Non-HDL*  TC/HDL*\n" +
		"  2025-03-15 00:30  195  110   55   118       140      3.5\n" +
		"mg/dL; * calculated (Friedewald LDL)\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCSV(&buf, []Record{fixedRecord(t)}); err != nil {
		t.Fatal(err)
	}
	want := "time,meter_time,total_cholesterol,triglycerides,hdl,ldl_calc,non_hdl_calc,tc_hdl_ratio,flag,raw\n" +
		"2025-03-15T00:30:00Z,2025-03-14T12:30:00Z,195,110,55,118,140,3.55,0x07,21 90 19 03 0e 08 1e 00 c3 00 6e 00 37 00 00 00 00 01 07 00 00\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, []Record{fixedRecord(t)}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"time": "2025-03-15T00:30:00Z", "meter_time": "2025-03-14T12:30:00Z",
		"total_cholesterol": 195.0, "triglycerides": 110.0, "hdl": 55.0,
		"ldl_calc": 118.0, "non_hdl_calc": 140.0, "flag": 7.0,
	}
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("%s = %v, want %v", k, rows[0][k], v)
		}
	}
}

// LDL is left out when triglycerides make Friedewald invalid.
func TestWriteJSON_NoLDLForHighTG(t *testing.T) {
	r := fixedRecord(t)
	r.TG = 450
	var buf bytes.Buffer
	if err := writeJSON(&buf, []Record{r}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ldl_calc") {
		t.Errorf("ldl_calc present:\n%s", buf.String())
	}
}
