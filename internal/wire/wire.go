// Package wire implements the Go side of the FerroGo wire format v1: the
// tagged, little-endian encoding of values crossing the Go/Rust boundary
// (docs/abi.md §5). Decoders are strict: unknown tags, truncated input,
// trailing bytes, oversized lengths and invalid UTF-8 are errors.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// Version is the args-header version byte.
const Version = 1

// DefaultMaxPayload bounds encoded arguments and results.
const DefaultMaxPayload = 16 << 20

// Call status codes (docs/abi.md §4).
const (
	StatusOK    uint32 = 0
	StatusError uint32 = 1
	StatusPanic uint32 = 2
	StatusABI   uint32 = 3
)

// Kind is a wire tag.
type Kind uint8

// Wire tags.
const (
	Void    Kind = 0x00
	Bool    Kind = 0x01
	Int32   Kind = 0x02
	Int64   Kind = 0x03
	Uint32  Kind = 0x04
	Uint64  Kind = 0x05
	Float32 Kind = 0x06
	Float64 Kind = 0x07
	String  Kind = 0x08
	Bytes   Kind = 0x09
	JSON    Kind = 0x0A
)

var kindNames = [...]string{"void", "bool", "i32", "i64", "u32", "u64", "f32", "f64", "String", "Vec<u8>", "Json"}

// String returns the Rust type name of the kind.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%#x)", uint8(k))
}

// Value is a decoded wire value. Num holds bools (0/1), integers (two's
// complement for signed kinds) and float bits; Data holds string, bytes and
// JSON payloads.
type Value struct {
	Kind Kind
	Num  uint64
	Data []byte
}

// ErrMalformed reports invalid wire data.
var ErrMalformed = errors.New("malformed wire data")

// EncodeArgs encodes an argument list. It fails if a string or JSON value is
// not valid UTF-8 (or JSON), if a kind is invalid, or if the encoding would
// exceed max bytes.
func EncodeArgs(args []Value, max int) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxPayload
	}
	size := 5
	for _, a := range args {
		size += 1 + payloadSize(a)
	}
	if size > max {
		return nil, fmt.Errorf("arguments encode to %d bytes; the limit is %d", size, max)
	}
	out := make([]byte, 0, size)
	out = append(out, Version)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(args)))
	for i, a := range args {
		var err error
		if out, err = appendValue(out, a); err != nil {
			return nil, fmt.Errorf("argument %d: %w", i, err)
		}
	}
	return out, nil
}

// EncodeValue encodes a single value (used for results in tests and by
// non-Rust peers).
func EncodeValue(v Value) ([]byte, error) {
	return appendValue(make([]byte, 0, 1+payloadSize(v)), v)
}

func payloadSize(v Value) int {
	switch v.Kind {
	case Bool:
		return 1
	case Int32, Uint32, Float32:
		return 4
	case Int64, Uint64, Float64:
		return 8
	case String, Bytes, JSON:
		return 4 + len(v.Data)
	}
	return 0
}

func appendValue(out []byte, v Value) ([]byte, error) {
	out = append(out, byte(v.Kind))
	switch v.Kind {
	case Void:
	case Bool:
		if v.Num > 1 {
			return nil, errors.New("invalid bool")
		}
		out = append(out, byte(v.Num))
	case Int32, Uint32, Float32:
		out = binary.LittleEndian.AppendUint32(out, uint32(v.Num))
	case Int64, Uint64, Float64:
		out = binary.LittleEndian.AppendUint64(out, v.Num)
	case String, JSON, Bytes:
		if v.Kind != Bytes && !utf8.Valid(v.Data) {
			return nil, fmt.Errorf("%s value is not valid UTF-8", v.Kind)
		}
		if v.Kind == JSON && !json.Valid(v.Data) {
			return nil, errors.New("JSON value is not valid JSON")
		}
		if uint64(len(v.Data)) > math.MaxUint32 {
			return nil, errors.New("value too large")
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(v.Data)))
		out = append(out, v.Data...)
	default:
		return nil, fmt.Errorf("invalid kind %#x", uint8(v.Kind))
	}
	return out, nil
}

// DecodeValue decodes exactly one value. The returned Data is a copy.
func DecodeValue(b []byte, max int) (Value, error) {
	if max <= 0 {
		max = DefaultMaxPayload
	}
	if len(b) > max {
		return Value{}, fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrMalformed, len(b), max)
	}
	if len(b) == 0 {
		return Value{}, fmt.Errorf("%w: empty", ErrMalformed)
	}
	v := Value{Kind: Kind(b[0])}
	rest := b[1:]
	need := func(n int) ([]byte, error) {
		if len(rest) < n {
			return nil, fmt.Errorf("%w: truncated %s", ErrMalformed, v.Kind)
		}
		p := rest[:n]
		rest = rest[n:]
		return p, nil
	}
	switch v.Kind {
	case Void:
	case Bool:
		p, err := need(1)
		if err != nil {
			return Value{}, err
		}
		if p[0] > 1 {
			return Value{}, fmt.Errorf("%w: invalid bool byte %d", ErrMalformed, p[0])
		}
		v.Num = uint64(p[0])
	case Int32, Uint32, Float32:
		p, err := need(4)
		if err != nil {
			return Value{}, err
		}
		v.Num = uint64(binary.LittleEndian.Uint32(p))
		if v.Kind == Int32 {
			v.Num = uint64(int64(int32(uint32(v.Num)))) // sign-extend
		}
	case Int64, Uint64, Float64:
		p, err := need(8)
		if err != nil {
			return Value{}, err
		}
		v.Num = binary.LittleEndian.Uint64(p)
	case String, Bytes, JSON:
		p, err := need(4)
		if err != nil {
			return Value{}, err
		}
		n := binary.LittleEndian.Uint32(p)
		if uint64(n) > uint64(len(rest)) {
			return Value{}, fmt.Errorf("%w: %s length %d exceeds remaining %d bytes", ErrMalformed, v.Kind, n, len(rest))
		}
		data, _ := need(int(n)) // length checked above
		if v.Kind != Bytes && !utf8.Valid(data) {
			return Value{}, fmt.Errorf("%w: %s value is not valid UTF-8", ErrMalformed, v.Kind)
		}
		if v.Kind == JSON && !json.Valid(data) {
			return Value{}, fmt.Errorf("%w: JSON value is not valid JSON", ErrMalformed)
		}
		v.Data = append([]byte(nil), data...)
	default:
		return Value{}, fmt.Errorf("%w: unknown tag %#x", ErrMalformed, uint8(v.Kind))
	}
	if len(rest) != 0 {
		return Value{}, fmt.Errorf("%w: %d trailing bytes", ErrMalformed, len(rest))
	}
	return v, nil
}

// DecodeArgs decodes an argument list (version, count, values) as written
// by EncodeArgs or by Rust host-function stubs.
func DecodeArgs(b []byte, max int) ([]Value, error) {
	if max <= 0 {
		max = DefaultMaxPayload
	}
	if len(b) > max {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrMalformed, len(b), max)
	}
	if len(b) < 5 || b[0] != Version {
		return nil, fmt.Errorf("%w: bad argument header", ErrMalformed)
	}
	count := binary.LittleEndian.Uint32(b[1:5])
	rest := b[5:]
	if uint64(count) > uint64(len(rest)) {
		return nil, fmt.Errorf("%w: %d arguments in %d bytes", ErrMalformed, count, len(rest))
	}
	out := make([]Value, 0, count)
	for i := range count {
		n, err := valueLen(rest)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i, err)
		}
		v, err := DecodeValue(rest[:n], max)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i, err)
		}
		out = append(out, v)
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrMalformed, len(rest))
	}
	return out, nil
}

// valueLen returns the encoded length of the value at the start of b.
func valueLen(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("%w: truncated", ErrMalformed)
	}
	var n int
	switch Kind(b[0]) {
	case Void:
		n = 1
	case Bool:
		n = 2
	case Int32, Uint32, Float32:
		n = 5
	case Int64, Uint64, Float64:
		n = 9
	case String, Bytes, JSON:
		if len(b) < 5 {
			return 0, fmt.Errorf("%w: truncated length", ErrMalformed)
		}
		l := binary.LittleEndian.Uint32(b[1:5])
		if uint64(l) > uint64(len(b)-5) {
			return 0, fmt.Errorf("%w: length %d exceeds input", ErrMalformed, l)
		}
		n = 5 + int(l)
	default:
		return 0, fmt.Errorf("%w: unknown tag %#x", ErrMalformed, b[0])
	}
	if n > len(b) {
		return 0, fmt.Errorf("%w: truncated", ErrMalformed)
	}
	return n, nil
}
