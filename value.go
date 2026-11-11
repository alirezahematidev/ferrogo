package ferrogo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// Kind identifies the type of a Value. Each kind corresponds to one Rust
// type at the call boundary (docs/abi.md §2).
type Kind uint8

// Value kinds.
const (
	KindVoid    Kind = Kind(wire.Void)    // Rust ()
	KindBool    Kind = Kind(wire.Bool)    // Rust bool
	KindInt32   Kind = Kind(wire.Int32)   // Rust i32
	KindInt64   Kind = Kind(wire.Int64)   // Rust i64
	KindUint32  Kind = Kind(wire.Uint32)  // Rust u32
	KindUint64  Kind = Kind(wire.Uint64)  // Rust u64
	KindFloat32 Kind = Kind(wire.Float32) // Rust f32
	KindFloat64 Kind = Kind(wire.Float64) // Rust f64
	KindString  Kind = Kind(wire.String)  // Rust String
	KindBytes   Kind = Kind(wire.Bytes)   // Rust Vec<u8>
	KindJSON    Kind = Kind(wire.JSON)    // Rust ferrogo::Json
)

// String returns the Rust type name for the kind ("i32", "String", ...).
func (k Kind) String() string { return wire.Kind(k).String() }

// Value is an immutable value passed to or returned from a Rust function.
// The zero Value is KindVoid. Create values with the constructors (Int32,
// String, JSON, ...); read them with the As* methods, which fail with
// ErrABIMismatch on a kind mismatch.
type Value struct{ v wire.Value }

// Void returns the unit value (Rust ()).
func Void() Value { return Value{} }

// Bool returns a bool value.
func Bool(b bool) Value {
	var n uint64
	if b {
		n = 1
	}
	return Value{wire.Value{Kind: wire.Bool, Num: n}}
}

// Int32 returns an i32 value.
func Int32(n int32) Value { return Value{wire.Value{Kind: wire.Int32, Num: uint64(int64(n))}} }

// Int64 returns an i64 value.
func Int64(n int64) Value { return Value{wire.Value{Kind: wire.Int64, Num: uint64(n)}} }

// Uint32 returns a u32 value.
func Uint32(n uint32) Value { return Value{wire.Value{Kind: wire.Uint32, Num: uint64(n)}} }

// Uint64 returns a u64 value.
func Uint64(n uint64) Value { return Value{wire.Value{Kind: wire.Uint64, Num: n}} }

// Float32 returns an f32 value.
func Float32(f float32) Value {
	return Value{wire.Value{Kind: wire.Float32, Num: uint64(math.Float32bits(f))}}
}

// Float64 returns an f64 value.
func Float64(f float64) Value { return Value{wire.Value{Kind: wire.Float64, Num: math.Float64bits(f)}} }

// String returns a String value. Strings must be valid UTF-8 (Rust
// requires it); Module.Call reports invalid UTF-8 as ErrABIMismatch.
func String(s string) Value { return Value{wire.Value{Kind: wire.String, Data: []byte(s)}} }

// Bytes returns a Vec<u8> value holding a copy of b.
func Bytes(b []byte) Value {
	return Value{wire.Value{Kind: wire.Bytes, Data: bytes.Clone(b)}}
}

// JSON marshals v with encoding/json into a JSON value (Rust ferrogo::Json),
// the way structs cross the boundary.
func JSON(v any) (Value, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return Value{}, fmt.Errorf("ferrogo: encoding JSON value: %w", err)
	}
	return Value{wire.Value{Kind: wire.JSON, Data: data}}, nil
}

// RawJSON returns a JSON value from already-encoded JSON text, which must be
// valid.
func RawJSON(data []byte) (Value, error) {
	if !json.Valid(data) {
		return Value{}, errors.New("ferrogo: RawJSON: invalid JSON")
	}
	return Value{wire.Value{Kind: wire.JSON, Data: bytes.Clone(data)}}, nil
}

// Kind returns the value's kind.
func (v Value) Kind() Kind { return Kind(v.v.Kind) }

func (v Value) want(k Kind) error {
	if v.Kind() != k {
		return fmt.Errorf("%w: value is %s, not %s", ErrABIMismatch, v.Kind(), k)
	}
	return nil
}

// AsBool returns the value of a KindBool value.
func (v Value) AsBool() (bool, error) { return v.v.Num == 1, v.want(KindBool) }

// AsInt32 returns the value of a KindInt32 value.
func (v Value) AsInt32() (int32, error) { return int32(v.v.Num), v.want(KindInt32) }

// AsInt64 returns the value of a KindInt64 value.
func (v Value) AsInt64() (int64, error) { return int64(v.v.Num), v.want(KindInt64) }

// AsUint32 returns the value of a KindUint32 value.
func (v Value) AsUint32() (uint32, error) { return uint32(v.v.Num), v.want(KindUint32) }

// AsUint64 returns the value of a KindUint64 value.
func (v Value) AsUint64() (uint64, error) { return v.v.Num, v.want(KindUint64) }

// AsFloat32 returns the value of a KindFloat32 value.
func (v Value) AsFloat32() (float32, error) {
	return math.Float32frombits(uint32(v.v.Num)), v.want(KindFloat32)
}

// AsFloat64 returns the value of a KindFloat64 value.
func (v Value) AsFloat64() (float64, error) {
	return math.Float64frombits(v.v.Num), v.want(KindFloat64)
}

// AsString returns the value of a KindString value.
func (v Value) AsString() (string, error) { return string(v.v.Data), v.want(KindString) }

// AsBytes returns a copy of the value of a KindBytes value.
func (v Value) AsBytes() ([]byte, error) { return bytes.Clone(v.v.Data), v.want(KindBytes) }

// AsJSON returns a copy of the JSON text of a KindJSON value.
func (v Value) AsJSON() ([]byte, error) { return bytes.Clone(v.v.Data), v.want(KindJSON) }

// Decode unmarshals a KindJSON value into dst with encoding/json.
func (v Value) Decode(dst any) error {
	if err := v.want(KindJSON); err != nil {
		return err
	}
	if err := json.Unmarshal(v.v.Data, dst); err != nil {
		return fmt.Errorf("ferrogo: decoding JSON value: %w", err)
	}
	return nil
}

// String formats the value for humans, e.g. `i32(30)` or `String("hi")`.
// It implements fmt.Stringer; use AsString to read a string value.
func (v Value) String() string {
	switch v.Kind() {
	case KindVoid:
		return "()"
	case KindBool:
		return strconv.FormatBool(v.v.Num == 1)
	case KindInt32, KindInt64:
		return fmt.Sprintf("%s(%d)", v.Kind(), int64(v.v.Num))
	case KindUint32, KindUint64:
		return fmt.Sprintf("%s(%d)", v.Kind(), v.v.Num)
	case KindFloat32:
		return fmt.Sprintf("f32(%v)", math.Float32frombits(uint32(v.v.Num)))
	case KindFloat64:
		return fmt.Sprintf("f64(%v)", math.Float64frombits(v.v.Num))
	case KindString:
		return fmt.Sprintf("String(%q)", v.v.Data)
	case KindJSON:
		return fmt.Sprintf("Json(%s)", v.v.Data)
	case KindBytes:
		return fmt.Sprintf("Vec<u8>(%d bytes)", len(v.v.Data))
	}
	return fmt.Sprintf("Value(kind=%d)", uint8(v.Kind()))
}

// validate checks constraints Rust enforces (UTF-8 strings).
func (v Value) validate() error {
	if v.Kind() == KindString && !utf8.Valid(v.v.Data) {
		return fmt.Errorf("%w: String value is not valid UTF-8", ErrABIMismatch)
	}
	return nil
}
