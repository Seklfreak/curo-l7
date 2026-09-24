package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Framing used by the CURO L7 (a rebadged SD BIOSENSOR LipidoCare). It is the
// SD CodeFree protocol with a different start byte and direction codes:
//
//	STX(0x73) DIR LEN MSG... XOR(MSG) ETX(0xAA)
//
// LEN counts MSG plus the checksum and ETX bytes.
const (
	stx      = 0x73
	etx      = 0xAA
	dirIn    = 0x50 // meter -> host
	dirOut   = 0x10 // host -> meter
	frameMin = 5    // STX DIR LEN CK ETX with an empty message
)

var (
	msgGreeting = []byte{0x10, 0x30} // sent by the meter at power-on
	msgAck      = []byte{0x10, 0x40} // host answer to the greeting
	msgNext     = []byte{0x10, 0x60} // request next record / end session
	msgBye      = []byte{0x10, 0x70} // meter: no more records
)

func checksum(msg []byte) byte {
	var c byte
	for _, b := range msg {
		c ^= b
	}
	return c
}

func buildFrame(msg []byte) []byte {
	f := make([]byte, 0, len(msg)+frameMin)
	f = append(f, stx, dirOut, byte(len(msg)+2))
	f = append(f, msg...)
	return append(f, checksum(msg), etx)
}

// parseFrame looks for one valid meter frame at the start of buf, skipping
// garbage (the meter emits a stray 0x00 at power-on). It returns the message,
// the number of bytes consumed, and ok=false when more data is needed.
func parseFrame(buf []byte) (msg []byte, consumed int, ok bool) {
	for i := 0; i < len(buf); i++ {
		if buf[i] != stx {
			continue
		}
		if len(buf)-i < 3 {
			return nil, i, false
		}
		n := int(buf[i+2])
		end := i + 3 + n
		if n < 2 || n > 64 || buf[i+1] != dirIn {
			continue
		}
		if end > len(buf) {
			return nil, i, false
		}
		m := buf[i+3 : end-2]
		if buf[end-1] != etx || buf[end-2] != checksum(m) {
			continue
		}
		return append([]byte(nil), m...), end, true
	}
	return nil, len(buf), false
}

// Record is one stored lipid measurement. Values are mg/dL.
type Record struct {
	MeterTime time.Time `json:"meter_time"` // as stored by the meter's clock
	Time      time.Time `json:"time"`       // MeterTime corrected by the clock offset
	TC        int       `json:"total_cholesterol"`
	TG        int       `json:"triglycerides"`
	HDL       int       `json:"hdl"`
	Unknown1  int       `json:"unknown_1"` // always 0 so far; likely glucose
	Unknown2  int       `json:"unknown_2"`
	Flag      byte      `json:"flag"` // 0x07 or 0x1f seen; meaning unknown
	Raw       string    `json:"raw"`
}

// Calculated values, as the meter displays them.

// LDL uses the Friedewald formula, which is invalid for TG >= 400.
func (r Record) LDL() (int, bool) {
	if r.TG >= 400 || r.HDL == 0 {
		return 0, false
	}
	return r.TC - r.HDL - (r.TG+2)/5, true
}

func (r Record) NonHDL() int { return r.TC - r.HDL }

func (r Record) Ratio() float64 {
	if r.HDL == 0 {
		return 0
	}
	return float64(r.TC) / float64(r.HDL)
}

const recordLen = 21

// parseRecord decodes a record message:
//
//	21 90 YY MM DD hh mm TC(u16) TG(u16) HDL(u16) ?(u16) ?(u16) 01 FLAG 00 00
func parseRecord(msg []byte, loc *time.Location) (Record, error) {
	if len(msg) != recordLen {
		return Record{}, fmt.Errorf("record: want %d bytes, got %d (% x)", recordLen, len(msg), msg)
	}
	if msg[0] != 0x21 || msg[1] != 0x90 {
		return Record{}, fmt.Errorf("record: unexpected header % x", msg[:2])
	}
	u16 := func(i int) int { return int(binary.BigEndian.Uint16(msg[i:])) }
	t := time.Date(2000+int(msg[2]), time.Month(msg[3]), int(msg[4]), int(msg[5]), int(msg[6]), 0, 0, loc)
	return Record{
		MeterTime: t,
		Time:      t,
		TC:        u16(7),
		TG:        u16(9),
		HDL:       u16(11),
		Unknown1:  u16(13),
		Unknown2:  u16(15),
		Flag:      msg[18],
		Raw:       fmt.Sprintf("% x", msg),
	}, nil
}

// parseCount decodes the reply to the greeting ack: 30 <count u16be> AA...
func parseCount(msg []byte) (int, error) {
	if len(msg) < 3 || msg[0] != 0x30 {
		return 0, fmt.Errorf("count: unexpected message % x", msg)
	}
	return int(binary.BigEndian.Uint16(msg[1:])), nil
}

var errTimeout = errors.New("timed out waiting for the meter")
