package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/gousb"
)

// The meter cable is a Prolific PL2303GC. macOS binds no driver to it, so it is
// driven from userspace: the HXN-family chips take CDC-style class requests for
// line coding and control lines.
const (
	plVendor  = 0x067b
	plProduct = 0x23a3

	epOut = 0x02
	epIn  = 0x83
)

type serialPort struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	done func()
	in   *gousb.InEndpoint
	out  *gousb.OutEndpoint
	buf  []byte
}

func openPort(baud uint32) (*serialPort, error) {
	ctx := gousb.NewContext()
	dev, err := ctx.OpenDeviceWithVIDPID(plVendor, plProduct)
	if err != nil || dev == nil {
		ctx.Close()
		if err == nil {
			err = errors.New("not found")
		}
		return nil, fmt.Errorf("open PL2303 cable (%04x:%04x): %w", plVendor, plProduct, err)
	}
	// No auto-detach: no kernel driver is bound, and on macOS asking libusb to
	// detach one fails with "bad access" without root.
	intf, done, err := dev.DefaultInterface()
	if err != nil {
		dev.Close()
		ctx.Close()
		return nil, fmt.Errorf("claim interface: %w", err)
	}
	p := &serialPort{ctx: ctx, dev: dev, done: done}
	if p.in, err = intf.InEndpoint(epIn & 0x7f); err == nil {
		p.out, err = intf.OutEndpoint(epOut)
	}
	if err == nil {
		err = p.configure(baud)
	}
	if err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *serialPort) configure(baud uint32) error {
	// SET_LINE_CODING: baud, 1 stop bit, no parity, 8 data bits.
	lc := make([]byte, 7)
	binary.LittleEndian.PutUint32(lc, baud)
	lc[6] = 8
	if _, err := p.dev.Control(0x21, 0x20, 0, 0, lc); err != nil {
		return fmt.Errorf("set line coding: %w", err)
	}
	// SET_CONTROL_LINE_STATE: raise DTR and RTS.
	if _, err := p.dev.Control(0x21, 0x22, 0x03, 0, nil); err != nil {
		return fmt.Errorf("set control lines: %w", err)
	}
	return nil
}

func (p *serialPort) Write(b []byte) error {
	_, err := p.out.Write(b)
	return err
}

// readFrame returns the next valid meter message, waiting up to timeout.
func (p *serialPort) readFrame(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	chunk := make([]byte, 64)
	for {
		msg, n, ok := parseFrame(p.buf)
		p.buf = p.buf[n:]
		if ok {
			return msg, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, errTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), min(left, 200*time.Millisecond))
		n, err := p.in.ReadContext(ctx, chunk)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, gousb.TransferCancelled) {
			return nil, fmt.Errorf("read: %w", err)
		}
		p.buf = append(p.buf, chunk[:n]...)
	}
}

func (p *serialPort) Close() {
	if p.done != nil {
		p.done()
	}
	p.dev.Close()
	p.ctx.Close()
}
