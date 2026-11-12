// SPDX-License-Identifier: GPL-3.0-or-later

package mihonbackup

import (
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// field is one decoded field of a message: its number, wire type and value
// (v for numbers, b for length-delimited values).
type field struct {
	num protowire.Number
	typ protowire.Type
	v   uint64
	b   []byte
}

// fields calls fn for every field of the message in b, in order. Fields fn
// doesn't use are simply ignored by it, so unknown fields are skipped.
func fields(b []byte, fn func(field) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return noEOF(protowire.ParseError(n))
		}
		b = b[n:]
		f := field{num: num, typ: typ}
		switch typ {
		case protowire.VarintType:
			f.v, n = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.v = uint64(v)
		case protowire.Fixed64Type:
			f.v, n = protowire.ConsumeFixed64(b)
		case protowire.BytesType:
			f.b, n = protowire.ConsumeBytes(b)
		default: // groups, which the format doesn't use
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return fmt.Errorf("field %d: %w", num, noEOF(protowire.ParseError(n)))
		}
		b = b[n:]
		if err := fn(f); err != nil {
			return err
		}
	}
	return nil
}

func (f field) want(typ protowire.Type) error {
	if f.typ != typ {
		return fmt.Errorf("field %d has wire type %d, want %d", f.num, f.typ, typ)
	}
	return nil
}

func (f field) int64() (int64, error) {
	return int64(f.v), f.want(protowire.VarintType) //nolint:gosec // two's complement, as encoded
}

func (f field) bool() (bool, error) {
	return f.v != 0, f.want(protowire.VarintType)
}

func (f field) float32() (float32, error) {
	return math.Float32frombits(uint32(f.v)), f.want(protowire.Fixed32Type) //nolint:gosec // a fixed32 value
}

// time reads a time in milliseconds since the epoch; 0 or less is unknown.
func (f field) time() (time.Time, error) {
	ms, err := f.int64()
	if err != nil || ms <= 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(ms).UTC(), nil
}

// string reads text. Invalid UTF-8 is replaced and NUL characters removed, as
// PostgreSQL text can hold neither.
func (f field) string() (string, error) {
	if err := f.want(protowire.BytesType); err != nil {
		return "", err
	}
	s := strings.ToValidUTF8(string(f.b), "�")
	return strings.ReplaceAll(s, "\x00", ""), nil
}

func (f field) message(fn func(field) error) error {
	if err := f.want(protowire.BytesType); err != nil {
		return err
	}
	return fields(f.b, fn)
}

// varints reads a repeated integer field, packed or not.
func (f field) varints(fn func(uint64) error) error {
	if f.typ == protowire.VarintType {
		return fn(f.v)
	}
	if err := f.want(protowire.BytesType); err != nil {
		return err
	}
	for b := f.b; len(b) > 0; {
		v, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return fmt.Errorf("field %d: %w", f.num, noEOF(protowire.ParseError(n)))
		}
		b = b[n:]
		if err := fn(v); err != nil {
			return err
		}
	}
	return nil
}
