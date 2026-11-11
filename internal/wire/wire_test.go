package wire

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func roundTrip(t *testing.T, v Value) Value {
	t.Helper()
	b, err := EncodeValue(v)
	if err != nil {
		t.Fatalf("encode %+v: %v", v, err)
	}
	got, err := DecodeValue(b, 0)
	if err != nil {
		t.Fatalf("decode %+v: %v", v, err)
	}
	return got
}

func TestRoundTrip(t *testing.T) {
	values := []Value{
		{Kind: Void},
		{Kind: Bool, Num: 1},
		{Kind: Int32, Num: bits(-42)},
		{Kind: Int64, Num: bits(math.MinInt64)},
		{Kind: Uint32, Num: math.MaxUint32},
		{Kind: Uint64, Num: math.MaxUint64},
		{Kind: Float32, Num: uint64(math.Float32bits(-1.5))},
		{Kind: Float64, Num: math.Float64bits(math.Pi)},
		{Kind: String, Data: []byte("héllo, 世界")},
		{Kind: String, Data: []byte{}},
		{Kind: Bytes, Data: []byte{0, 0xff, 0x80}},
		{Kind: JSON, Data: []byte(`{"a":[1,2,{"b":null}]}`)},
	}
	for _, v := range values {
		got := roundTrip(t, v)
		if got.Kind != v.Kind || got.Num != v.Num || !bytes.Equal(got.Data, v.Data) {
			t.Errorf("round trip %+v = %+v", v, got)
		}
	}
}

func TestEncodeArgsHeaderAndLayout(t *testing.T) {
	b, err := EncodeArgs([]Value{{Kind: Int32, Num: 10}, {Kind: String, Data: []byte("hi")}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 0, 0, 0, byte(Int32), 10, 0, 0, 0, byte(String), 2, 0, 0, 0, 'h', 'i'}
	if !bytes.Equal(b, want) {
		t.Errorf("encoding = %v\nwant       %v", b, want)
	}
}

func TestEncodeRejectsInvalidInput(t *testing.T) {
	bad := []Value{
		{Kind: String, Data: []byte{0xff, 0xfe}},
		{Kind: JSON, Data: []byte(`{"unterminated":`)},
		{Kind: Bool, Num: 2},
		{Kind: Kind(0x7f)},
	}
	for _, v := range bad {
		if _, err := EncodeArgs([]Value{v}, 0); err == nil {
			t.Errorf("accepted %+v", v)
		}
	}
	if _, err := EncodeArgs([]Value{{Kind: Bytes, Data: make([]byte, 100)}}, 50); err == nil {
		t.Error("payload limit not enforced")
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"unknown tag":    {0x7f},
		"truncated i32":  {byte(Int32), 1, 2},
		"trailing":       {byte(Void), 0},
		"bad bool":       {byte(Bool), 2},
		"length overrun": {byte(String), 10, 0, 0, 0, 'a'},
		"huge length":    {byte(Bytes), 0xff, 0xff, 0xff, 0xff},
		"invalid utf8":   {byte(String), 1, 0, 0, 0, 0xff},
		"invalid json":   {byte(JSON), 1, 0, 0, 0, '{'},
	}
	for name, b := range cases {
		if _, err := DecodeValue(b, 0); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	if _, err := DecodeValue(make([]byte, 64), 16); err == nil {
		t.Error("max payload not enforced on decode")
	}
}

func TestDecodeCopiesData(t *testing.T) {
	b := []byte{byte(Bytes), 3, 0, 0, 0, 'a', 'b', 'c'}
	v, err := DecodeValue(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	b[5] = 'X' // the source buffer is guest memory that will be freed
	if string(v.Data) != "abc" {
		t.Error("decoded data aliases the input buffer")
	}
}

func FuzzDecodeValue(f *testing.F) {
	for _, seed := range [][]byte{
		{byte(Void)}, {byte(Bool), 1}, {byte(Int32), 1, 2, 3, 4},
		{byte(String), 2, 0, 0, 0, 'h', 'i'}, {byte(JSON), 2, 0, 0, 0, '{', '}'},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := DecodeValue(b, 1<<16)
		if err != nil {
			return
		}
		// Anything that decodes must re-encode to the same bytes.
		again, err := EncodeValue(v)
		if err != nil {
			t.Fatalf("decoded value %+v does not re-encode: %v", v, err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("re-encoding differs:\n in %v\nout %v", b, again)
		}
	})
}

// bits returns the two's-complement bits of v (avoids constant overflow).
func bits(v int64) uint64 { return uint64(v) }

func TestDecodeArgsRoundTrip(t *testing.T) {
	args := []Value{{Kind: Int32, Num: bits(-3)}, {Kind: String, Data: []byte("hé")}, {Kind: Void}, {Kind: Bytes, Data: []byte{9}}}
	b, err := EncodeArgs(args, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeArgs(b, 0)
	if err != nil || len(got) != 4 || got[0].Num != args[0].Num || string(got[1].Data) != "hé" || got[3].Data[0] != 9 {
		t.Fatalf("DecodeArgs = %+v, %v", got, err)
	}
	for _, bad := range [][]byte{nil, {2, 0, 0, 0, 0}, {1, 9, 0, 0, 0}, append(b, 0)} {
		if _, err := DecodeArgs(bad, 0); err == nil {
			t.Errorf("DecodeArgs(%v) accepted", bad)
		}
	}
}

func FuzzDecodeArgs(f *testing.F) {
	b, _ := EncodeArgs([]Value{{Kind: Int32, Num: 1}, {Kind: String, Data: []byte("x")}}, 0)
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		args, err := DecodeArgs(b, 1<<16)
		if err != nil {
			return
		}
		again, err := EncodeArgs(args, 1<<16)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("re-encoding differs: %v %v", err, again)
		}
	})
}
