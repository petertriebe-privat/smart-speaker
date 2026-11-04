package wakeword

import (
	"encoding/binary"
	"math"
)

// A minimal FlatBuffers reader for the .tflite schema: tables, scalars, vectors and strings.

type table struct {
	b   []byte
	pos int
}

func u32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i:]) }

func rootTable(b []byte) table { return table{b, int(u32(b, 0))} }

// field: the absolute position of field n, 0 if absent.
func (t table) field(n int) int {
	vt := t.pos - int(int32(u32(t.b, t.pos)))
	vtSize := int(binary.LittleEndian.Uint16(t.b[vt:]))
	at := 4 + 2*n
	if at >= vtSize {
		return 0
	}
	off := int(binary.LittleEndian.Uint16(t.b[vt+at:]))
	if off == 0 {
		return 0
	}
	return t.pos + off
}

func (t table) i32(n int, def int32) int32 {
	if p := t.field(n); p != 0 {
		return int32(u32(t.b, p))
	}
	return def
}

func (t table) u8(n int, def uint8) uint8 {
	if p := t.field(n); p != 0 {
		return t.b[p]
	}
	return def
}

func (t table) i8(n int, def int8) int8 { return int8(t.u8(n, uint8(def))) }

// indirect: the position an offset field points to.
func (t table) indirect(p int) int { return p + int(u32(t.b, p)) }

func (t table) sub(n int) (table, bool) {
	p := t.field(n)
	if p == 0 {
		return table{}, false
	}
	return table{t.b, t.indirect(p)}, true
}

// vector: start of the elements and their count.
func (t table) vector(n int) (int, int) {
	p := t.field(n)
	if p == 0 {
		return 0, 0
	}
	v := t.indirect(p)
	return v + 4, int(u32(t.b, v))
}

func (t table) str(n int) string {
	start, count := t.vector(n)
	return string(t.b[start : start+count])
}

func (t table) tables(n int) []table {
	start, count := t.vector(n)
	out := make([]table, count)
	for i := range out {
		p := start + 4*i
		out[i] = table{t.b, t.indirect(p)}
	}
	return out
}

func (t table) ints(n int) []int {
	start, count := t.vector(n)
	out := make([]int, count)
	for i := range out {
		out[i] = int(int32(u32(t.b, start+4*i)))
	}
	return out
}

func (t table) floats(n int) []float32 {
	start, count := t.vector(n)
	out := make([]float32, count)
	for i := range out {
		out[i] = math.Float32frombits(u32(t.b, start+4*i))
	}
	return out
}

func (t table) int64s(n int) []int64 {
	start, count := t.vector(n)
	out := make([]int64, count)
	for i := range out {
		out[i] = int64(binary.LittleEndian.Uint64(t.b[start+8*i:]))
	}
	return out
}

func (t table) bytes(n int) []byte {
	start, count := t.vector(n)
	return t.b[start : start+count]
}
