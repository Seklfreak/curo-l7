// Command curo-l7 downloads the stored results from a CURO L7 lipid meter over
// its USB cable, and can import them into lab-tracker (`curo-l7 import`).
//
// The meter only talks right after it is switched on: start the tool, then
// turn the meter on with the cable plugged in.
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"text/tabwriter"
	"time"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) > 1 && os.Args[1] == "import" {
		if err := runImport(os.Args[2:], os.Stdin, os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			log.Fatal(err)
		}
		return
	}

	var (
		format      = flag.String("format", "table", "output format: table, csv or json")
		wait        = flag.Duration("wait", 5*time.Minute, "how long to wait for the meter to be switched on")
		clockOffset = flag.Duration("clock-offset", 12*time.Hour, "added to the meter's timestamps (this meter's clock runs 12h behind)")
		verbose     = flag.Bool("v", false, "log the exchanged frames")
	)
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: curo-l7 [flags]           print the meter's results\n       curo-l7 import [flags]    import them into lab-tracker (see curo-l7 import -h)")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *format != "table" && *format != "csv" && *format != "json" {
		log.Fatalf("unknown -format %q", *format)
	}

	port, err := openPort(38400)
	if err != nil {
		log.Fatal(err)
	}
	defer port.Close()

	recs, err := download(port, *wait, *verbose)
	if err != nil {
		log.Fatal(err)
	}
	for i := range recs {
		recs[i].Time = recs[i].MeterTime.Add(*clockOffset)
	}

	switch *format {
	case "json":
		err = writeJSON(os.Stdout, recs)
	case "csv":
		err = writeCSV(os.Stdout, recs)
	default:
		err = writeTable(os.Stdout, recs)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// meterConn is the framed link to the meter; *serialPort in production, a
// scripted fake in tests.
type meterConn interface {
	Write(frame []byte) error
	readFrame(timeout time.Duration) ([]byte, error)
}

// ackTimeout is how long the meter gets to answer our reply to its greeting.
var ackTimeout = 5 * time.Second

func download(p meterConn, wait time.Duration, verbose bool) ([]Record, error) {
	send := func(msg []byte) error {
		f := buildFrame(msg)
		if verbose {
			log.Printf("TX % x", f)
		}
		return p.Write(f)
	}
	recv := func(timeout time.Duration) ([]byte, error) {
		msg, err := p.readFrame(timeout)
		if verbose && err == nil {
			log.Printf("RX % x", msg)
		}
		return msg, err
	}

	log.Printf("Switch the meter on now (waiting up to %s)...", wait)
	deadline := time.Now().Add(wait)
	var count int
	var msg []byte // a greeting already received, if any
	for {
		if msg == nil {
			var err error
			if msg, err = recv(time.Until(deadline)); err != nil {
				return nil, fmt.Errorf("waiting for greeting: %w", err)
			}
		}
		if !bytes.Equal(msg, msgGreeting) {
			msg = nil
			continue
		}
		// The meter gives up within seconds, so answer immediately.
		if err := send(msgAck); err != nil {
			return nil, err
		}
		reply, err := recv(ackTimeout)
		if err != nil {
			// Usually a greeting we saw too late. The meter goes quiet until
			// it's power-cycled, so wait for the next greeting.
			log.Printf("The meter didn't answer. Switch it off and on again...")
			msg = nil
			continue
		}
		if bytes.Equal(reply, msgGreeting) {
			msg = reply // power-cycled while we waited: answer the new greeting
			continue
		}
		if count, err = parseCount(reply); err != nil {
			return nil, err
		}
		break
	}
	log.Printf("Meter holds %d record(s).", count)

	recs := make([]Record, 0, count)
	for i := 0; i < count; i++ {
		if err := send(msgNext); err != nil {
			return nil, err
		}
		msg, err := recv(5 * time.Second)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i+1, err)
		}
		r, err := parseRecord(msg, time.Local)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i+1, err)
		}
		recs = append(recs, r)
	}
	// Close the session; the meter answers with 10 70.
	if err := send(msgNext); err == nil {
		if msg, err := recv(2 * time.Second); err == nil && !bytes.Equal(msg, msgBye) && verbose {
			log.Printf("unexpected end-of-session reply % x", msg)
		}
	}
	return recs, nil
}

const timeLayout = "2006-01-02 15:04"

func ldlString(r Record) string {
	if v, ok := r.LDL(); ok {
		return strconv.Itoa(v)
	}
	return ""
}

func writeTable(w io.Writer, recs []Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "Time\tTC\tTG\tHDL\tLDL*\tNon-HDL*\tTC/HDL*\t")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%d\t%.1f\t\n",
			r.Time.Format(timeLayout), r.TC, r.TG, r.HDL, ldlString(r), r.NonHDL(), r.Ratio())
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "mg/dL; * calculated (Friedewald LDL)")
	return err
}

func writeCSV(w io.Writer, recs []Record) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"time", "meter_time", "total_cholesterol", "triglycerides", "hdl", "ldl_calc", "non_hdl_calc", "tc_hdl_ratio", "flag", "raw"})
	for _, r := range recs {
		cw.Write([]string{
			r.Time.Format(time.RFC3339), r.MeterTime.Format(time.RFC3339),
			strconv.Itoa(r.TC), strconv.Itoa(r.TG), strconv.Itoa(r.HDL),
			ldlString(r), strconv.Itoa(r.NonHDL()), strconv.FormatFloat(r.Ratio(), 'f', 2, 64),
			fmt.Sprintf("0x%02x", r.Flag), r.Raw,
		})
	}
	cw.Flush()
	return cw.Error()
}

func writeJSON(w io.Writer, recs []Record) error {
	type out struct {
		Record
		LDL    *int    `json:"ldl_calc,omitempty"`
		NonHDL int     `json:"non_hdl_calc"`
		Ratio  float64 `json:"tc_hdl_ratio"`
	}
	rows := make([]out, len(recs))
	for i, r := range recs {
		rows[i] = out{Record: r, NonHDL: r.NonHDL(), Ratio: r.Ratio()}
		if v, ok := r.LDL(); ok {
			rows[i].LDL = &v
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
