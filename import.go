package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const deviceName = "CURO L7"

// externalID is the stable identity of a reading in lab-tracker: the raw record
// bytes (timestamp to the minute plus all values), independent of any clock
// correction applied on this side.
func externalID(r Record) string {
	return "curo-l7:" + strings.ReplaceAll(r.Raw, " ", "")
}

// toLabTracker maps a record to the measured analytes. Calculated values (LDL,
// non-HDL, ratio) are deliberately not sent. Zero means "not measured".
func toLabTracker(r Record, profileID string) ltReading {
	reading := ltReading{
		ExternalID: externalID(r),
		ProfileID:  profileID,
		TakenAt:    r.Time.Format(time.RFC3339),
		Device:     deviceName,
	}
	for _, v := range []struct {
		name  string
		value int
	}{{"Total Cholesterol", r.TC}, {"Triglycerides", r.TG}, {"HDL Cholesterol", r.HDL}} {
		if v.value > 0 {
			reading.Results = append(reading.Results, ltResult{Analyte: v.name, Value: float64(v.value), Unit: "mg/dL"})
		}
	}
	return reading
}

type importConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// loadConfig reads LAB_TRACKER_URL / LAB_TRACKER_TOKEN, falling back to
// ~/.config/curo-l7/config.json.
func loadConfig() (importConfig, error) {
	var cfg importConfig
	if dir, err := os.UserConfigDir(); err == nil {
		// os.UserConfigDir is ~/Library/Application Support on macOS; prefer the
		// XDG-style path that's easier to find and edit.
		for _, p := range []string{
			filepath.Join(os.Getenv("HOME"), ".config", "curo-l7", "config.json"),
			filepath.Join(dir, "curo-l7", "config.json"),
		} {
			if b, err := os.ReadFile(p); err == nil {
				if err := json.Unmarshal(b, &cfg); err != nil {
					return cfg, fmt.Errorf("%s: %w", p, err)
				}
				break
			}
		}
	}
	if v := os.Getenv("LAB_TRACKER_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("LAB_TRACKER_TOKEN"); v != "" {
		cfg.Token = v
	}
	if cfg.URL == "" || cfg.Token == "" {
		return cfg, errors.New("set LAB_TRACKER_URL and LAB_TRACKER_TOKEN (or url/token in ~/.config/curo-l7/config.json); create a token on lab-tracker's Tokens page")
	}
	return cfg, nil
}

// readRecordsFile loads records previously saved with -format json, re-decoding
// each from its raw bytes.
func readRecordsFile(path string) ([]Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	recs := make([]Record, 0, len(rows))
	for i, row := range rows {
		raw, err := parseHex(row.Raw)
		if err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", path, i+1, err)
		}
		rec, err := parseRecord(raw, time.Local)
		if err != nil {
			return nil, fmt.Errorf("%s: record %d: %w", path, i+1, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func parseHex(s string) ([]byte, error) {
	fields := strings.Fields(s)
	out := make([]byte, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			return nil, fmt.Errorf("bad hex byte %q", f)
		}
		out[i] = byte(v)
	}
	return out, nil
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	var (
		in          = fs.String("in", "", "import records saved with -format json instead of reading the meter")
		wait        = fs.Duration("wait", 5*time.Minute, "how long to wait for the meter to be switched on")
		clockOffset = fs.Duration("clock-offset", 12*time.Hour, "added to the meter's timestamps (this meter's clock runs 12h behind)")
		verbose     = fs.Bool("v", false, "log the exchanged frames")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: curo-l7 import [flags]\n\nReads the meter (or -in file), skips readings lab-tracker already has, asks\nwhich profile each new reading belongs to, and uploads them.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	lt := newLabTracker(cfg.URL, cfg.Token)
	ctx := context.Background()

	// Fail on a bad URL/token before asking the user to switch the meter on.
	profiles, err := lt.profiles(ctx)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		return errors.New("no lab-tracker profiles are available to this token")
	}

	var recs []Record
	if *in != "" {
		recs, err = readRecordsFile(*in)
	} else {
		var port *serialPort
		if port, err = openPort(38400); err != nil {
			return err
		}
		recs, err = download(port, *wait, *verbose)
		port.Close()
	}
	if err != nil {
		return err
	}
	for i := range recs {
		recs[i].Time = recs[i].MeterTime.Add(*clockOffset)
	}

	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = externalID(r)
	}
	known, err := lt.imported(ctx, ids)
	if err != nil {
		return err
	}
	var fresh []Record
	for _, r := range recs {
		if _, ok := known[externalID(r)]; !ok {
			fresh = append(fresh, r)
		}
	}
	log.Printf("%d reading(s): %d already in lab-tracker, %d new.", len(recs), len(recs)-len(fresh), len(fresh))
	if len(fresh) == 0 {
		return nil
	}

	names := make(map[string]string, len(profiles))
	for _, p := range profiles {
		names[p.ID] = p.Name
	}
	imported, skipped := 0, 0
	stdin := bufio.NewReader(os.Stdin)
	last := -1
	for _, r := range fresh {
		reading := toLabTracker(r, "")
		if len(reading.Results) == 0 {
			log.Printf("%s: no measured values, skipping", r.Time.Format(timeLayout))
			skipped++
			continue
		}
		choice, err := askProfile(os.Stderr, stdin, r, profiles, last)
		if errors.Is(err, errQuit) {
			break
		}
		if err != nil {
			return err
		}
		if choice < 0 {
			skipped++
			continue
		}
		last = choice
		reading.ProfileID = profiles[choice].ID
		resp, err := lt.importReading(ctx, reading)
		if err != nil {
			return err
		}
		if resp.Status == "exists" {
			log.Printf("  already in lab-tracker (%s)", names[resp.ProfileID])
			continue
		}
		log.Printf("  imported for %s", profiles[choice].Name)
		imported++
	}
	log.Printf("Done: %d imported, %d skipped.", imported, skipped)
	return nil
}

var errQuit = errors.New("quit")

// askProfile prompts for the profile a reading belongs to. It returns the
// profile index, -1 to skip, or errQuit. Enter picks the previous choice.
func askProfile(w io.Writer, in *bufio.Reader, r Record, profiles []ltProfile, last int) (int, error) {
	fmt.Fprintf(w, "\n%s  TC %d  TG %d  HDL %d mg/dL\n", r.Time.Format(timeLayout), r.TC, r.TG, r.HDL)
	for i, p := range profiles {
		mark := " "
		if i == last {
			mark = "*"
		}
		fmt.Fprintf(w, " %s[%d] %s\n", mark, i+1, p.Name)
	}
	for {
		prompt := "Profile number, s to skip, q to quit"
		if last >= 0 {
			prompt += fmt.Sprintf(" [Enter = %s]", profiles[last].Name)
		}
		fmt.Fprint(w, prompt+": ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			if errors.Is(err, io.EOF) {
				return 0, errQuit
			}
			return 0, err
		}
		switch s := strings.ToLower(strings.TrimSpace(line)); {
		case s == "" && last >= 0:
			return last, nil
		case s == "s":
			return -1, nil
		case s == "q":
			return 0, errQuit
		default:
			if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(profiles) {
				return n - 1, nil
			}
		}
		fmt.Fprintln(w, "Please enter one of the numbers above.")
	}
}
