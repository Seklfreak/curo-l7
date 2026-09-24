# curo-l7

Downloads the stored results from a CURO L7 lipid meter over its USB cable.

The CURO L7 is a rebadged SD BIOSENSOR LipidoCare. Its cable is a Prolific
PL2303GC USB-serial adapter, which macOS has no driver for, so the tool drives
it from userspace through libusb.

## Build

```
brew install libusb pkg-config
go build -o curo-l7 .
```

## Use

Start the tool, then switch the meter on with the cable plugged in. The meter
only talks for a few seconds after power-on.

```
./curo-l7                 # table
./curo-l7 -format csv     # or json
./curo-l7 -v              # also log every frame
```

`-clock-offset` (default `12h`) is added to the meter's timestamps, because
this meter's clock runs 12 hours behind. Use `-clock-offset 0` for a meter
with a correct clock. CSV and JSON keep the meter's own timestamp as well.

LDL (Friedewald), non-HDL and the TC/HDL ratio are calculated; the meter only
stores total cholesterol, triglycerides and HDL.

## Protocol

It's the SD CodeFree protocol documented by
[glucometerutils](https://github.com/glucometers-tech/glucometerutils), with
a different start byte and direction codes.

- 38400 baud, 8N1.
- Frame: `73 DIR LEN MSG… XOR(MSG) AA`. `DIR` is `50` from the meter and `10`
  from the host; `LEN` counts the message plus the checksum and `AA`.
- At power-on the meter sends `10 30` (after a stray `00`). The host must
  answer `10 40` within a few seconds. The meter replies `30 <count u16be>`
  followed by `AA` padding.
- The host sends `10 60` once per record. One more `10 60` gets `10 70`, which
  ends the session.
- Record, newest first:
  `21 90 YY MM DD hh mm TC TG HDL ?? ?? 01 FLAG 00 00`. Values are big-endian
  u16 in mg/dL. The two unknown slots have always been 0 so far (probably
  glucose). `FLAG` has been `07` or `1f`; its meaning is unknown.
