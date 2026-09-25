package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // download/runImport narrate progress via log
	os.Exit(m.Run())
}

// fakeMeter plays the meter's side of a session. Queued messages come back
// from readFrame; an empty queue behaves like a timeout.
type fakeMeter struct {
	t       *testing.T
	queue   [][]byte
	records [][]byte
	next    int
	// ignoreAcks makes the meter ignore that many acks, as when our reply
	// comes too late. It then gets power-cycled and greets again: after one
	// timeout, or at once if quickGreet is set.
	ignoreAcks int
	quickGreet bool
	rebooting  bool
	sent       [][]byte
}

func (m *fakeMeter) readFrame(time.Duration) ([]byte, error) {
	if len(m.queue) == 0 {
		if m.rebooting {
			m.rebooting = false
			m.queue = append(m.queue, msgGreeting)
		}
		return nil, errTimeout
	}
	msg := m.queue[0]
	m.queue = m.queue[1:]
	return msg, nil
}

func (m *fakeMeter) Write(f []byte) error {
	m.t.Helper()
	// Validate the host's framing: STX, host direction, length, checksum, ETX.
	if len(f) < frameMin || f[0] != stx || f[1] != dirOut || int(f[2]) != len(f)-3 || f[len(f)-1] != etx {
		m.t.Fatalf("malformed host frame % x", f)
	}
	msg := f[3 : len(f)-2]
	if f[len(f)-2] != checksum(msg) {
		m.t.Fatalf("bad checksum in % x", f)
	}
	m.sent = append(m.sent, append([]byte(nil), msg...))

	switch {
	case bytes.Equal(msg, msgAck):
		if m.ignoreAcks > 0 {
			m.ignoreAcks--
			if m.quickGreet {
				m.queue = append(m.queue, msgGreeting)
			} else {
				m.rebooting = true
			}
			return nil
		}
		count := []byte{0x30, 0, byte(len(m.records))}
		m.queue = append(m.queue, append(count, bytes.Repeat([]byte{0xAA}, 19)...))
	case bytes.Equal(msg, msgNext):
		if m.next < len(m.records) {
			m.queue = append(m.queue, m.records[m.next])
			m.next++
		} else {
			m.queue = append(m.queue, msgBye)
		}
	default:
		m.t.Fatalf("unexpected host message % x", msg)
	}
	return nil
}

// recordMsgs are the made-up records from protocol_test.go, as messages.
func recordMsgs(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	for _, f := range []string{
		"73 50 17 21 90 19 03 0e 08 1e 00 c3 00 6e 00 37 00 00 00 00 01 07 00 00 2f aa",
		"73 50 17 21 90 19 03 0e 08 15 00 96 00 3e 00 30 00 00 00 00 01 07 00 00 26 aa",
	} {
		msg, _, ok := parseFrame(unhex(t, f))
		if !ok {
			t.Fatalf("bad fixture %s", f)
		}
		out = append(out, msg)
	}
	return out
}

func TestDownload_FullSession(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}, records: recordMsgs(t)}
	recs, err := download(m, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].TC != 195 || recs[1].TC != 150 {
		t.Fatalf("records %+v", recs)
	}
	// ack, one request per record, and the closing request.
	want := [][]byte{msgAck, msgNext, msgNext, msgNext}
	if len(m.sent) != len(want) {
		t.Fatalf("sent %d messages, want %d", len(m.sent), len(want))
	}
	for i := range want {
		if !bytes.Equal(m.sent[i], want[i]) {
			t.Errorf("message %d = % x, want % x", i, m.sent[i], want[i])
		}
	}
}

func TestDownload_EmptyMeter(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}}
	recs, err := download(m, time.Minute, false)
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
}

// A greeting we answer too late gets no reply; the tool must wait for the next
// power-on instead of giving up (the 2026-09-24 failure).
func TestDownload_RetriesAfterIgnoredAck(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}, records: recordMsgs(t), ignoreAcks: 2}
	recs, err := download(m, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records", len(recs))
	}
	acks := 0
	for _, msg := range m.sent {
		if bytes.Equal(msg, msgAck) {
			acks++
		}
	}
	if acks != 3 {
		t.Errorf("sent %d acks, want 3", acks)
	}
}

// Switched off and on again while we waited for the count: the new greeting
// arrives in place of the count and must be answered, not treated as an error.
func TestDownload_GreetingInsteadOfCount(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}, records: recordMsgs(t), ignoreAcks: 1, quickGreet: true}
	recs, err := download(m, time.Minute, false)
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
}

// Anything before the greeting (e.g. a leftover end-of-session reply) is ignored.
func TestDownload_IgnoresMessagesBeforeGreeting(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgBye, {0x30, 0x00}, msgGreeting}, records: recordMsgs(t)[:1]}
	recs, err := download(m, time.Minute, false)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	if !bytes.Equal(m.sent[0], msgAck) {
		t.Errorf("first message % x, want the ack", m.sent[0])
	}
}

func TestDownload_NoGreeting(t *testing.T) {
	m := &fakeMeter{t: t}
	_, err := download(m, time.Minute, false)
	if !errors.Is(err, errTimeout) || !strings.Contains(err.Error(), "greeting") {
		t.Fatalf("err = %v", err)
	}
	if len(m.sent) != 0 {
		t.Errorf("sent % x without a greeting", m.sent)
	}
}

func TestDownload_BadRecord(t *testing.T) {
	bad := append([]byte{0x22, 0x90}, make([]byte, recordLen-2)...)
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}, records: [][]byte{bad}}
	_, err := download(m, time.Minute, false)
	if err == nil || !strings.Contains(err.Error(), "record 1") {
		t.Fatalf("err = %v", err)
	}
}

// The meter claims more records than it sends: report which one is missing.
func TestDownload_MissingRecord(t *testing.T) {
	m := &fakeMeter{t: t, queue: [][]byte{msgGreeting}, records: recordMsgs(t)[:1]}
	_, err := download(&countLiar{fakeMeter: m, claim: 2}, time.Minute, false)
	if err == nil || !strings.Contains(err.Error(), "record 2") {
		t.Fatalf("err = %v", err)
	}
}

// countLiar reports more records than the fake meter holds.
type countLiar struct {
	*fakeMeter
	claim byte
}

func (c *countLiar) readFrame(d time.Duration) ([]byte, error) {
	msg, err := c.fakeMeter.readFrame(d)
	if err == nil && len(msg) > 2 && msg[0] == 0x30 {
		msg = append([]byte(nil), msg...)
		msg[2] = c.claim
	}
	return msg, err
}
