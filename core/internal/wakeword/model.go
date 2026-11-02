package wakeword

import (
	"encoding/binary"
	"fmt"
	"math"
)

// A small int8 TFLite interpreter for microWakeWord's streaming models: exactly the operators they
// use (CONV_2D, DEPTHWISE_CONV_2D, FULLY_CONNECTED, CONCATENATION, STRIDED_SLICE, SPLIT_V, RESHAPE, the
// resource variables that keep the streaming state, LOGISTIC, QUANTIZE), with TFLite Micro's
// reference integer arithmetic. A model with another operator is refused at load.

const (
	opConcat         = 2
	opConv           = 3
	opDepthwise      = 4
	opFullyConn      = 9
	opLogistic       = 14
	opReshape        = 22
	opStridedSlice   = 45
	opSplitV         = 102
	opQuantize       = 114
	opCallOnce       = 129
	opVarHandle      = 142
	opReadVariable   = 143
	opAssignVariable = 144

	typeInt32    = 2
	typeUint8    = 3
	typeInt8     = 9
	typeResource = 13
)

type tensor struct {
	shape []int
	typ   int
	i8    []int8
	i32   []int32
	u8    []uint8
	scale []float32
	zp    []int64
	res   int // resource variables: which one (-1 = not a handle)
}

func (t *tensor) size() int {
	n := 1
	for _, d := range t.shape {
		n *= d
	}
	return n
}

func (t *tensor) zero() int32 {
	if len(t.zp) == 0 {
		return 0
	}
	return int32(t.zp[0])
}

type operator struct {
	code    int
	in, out []int
	opts    table
	hasOpts bool
	// precomputed
	mult   []int32
	shift  []int
	actMin int32
	actMax int32
}

type subgraph struct {
	tensors []*tensor
	ops     []*operator
	inputs  []int
	outputs []int
}

// Model is one loaded wake word model with its streaming state.
type Model struct {
	graphs    []*subgraph
	variables map[string][]int8 // the streaming state by shared name
	names     []string          // resource handles -> shared name
	called    map[int]bool      // CALL_ONCE: subgraphs run already
}

// LoadModel parses a .tflite file.
func LoadModel(data []byte) (*Model, error) {
	root := rootTable(data)
	codes := []int{}
	for _, c := range root.tables(1) {
		code := int(c.i32(3, 0))
		if dep := int(c.i8(0, 0)); dep > code {
			code = dep
		}
		codes = append(codes, code)
	}
	buffers := root.tables(4)
	m := &Model{variables: map[string][]int8{}, called: map[int]bool{}}
	for _, g := range root.tables(2) {
		sg := &subgraph{inputs: g.ints(1), outputs: g.ints(2)}
		for _, t := range g.tables(0) {
			ten := &tensor{shape: t.ints(0), typ: int(t.i8(1, 0)), res: -1}
			if q, ok := t.sub(4); ok {
				ten.scale, ten.zp = q.floats(2), q.int64s(3)
			}
			var raw []byte
			if b := int(uint32(t.i32(2, 0))); b < len(buffers) {
				raw = buffers[b].bytes(0)
			}
			n := ten.size()
			switch ten.typ {
			case typeInt8:
				ten.i8 = make([]int8, n)
				for i := 0; i < len(raw) && i < n; i++ {
					ten.i8[i] = int8(raw[i])
				}
			case typeUint8:
				ten.u8 = make([]uint8, n)
				copy(ten.u8, raw)
			case typeInt32:
				ten.i32 = make([]int32, n)
				for i := 0; i < n && 4*i+4 <= len(raw); i++ {
					ten.i32[i] = int32(binary.LittleEndian.Uint32(raw[4*i:]))
				}
			case typeResource:
			default:
				return nil, fmt.Errorf("tensor type %d not supported", ten.typ)
			}
			sg.tensors = append(sg.tensors, ten)
		}
		for _, o := range g.tables(3) {
			op := &operator{code: codes[o.i32(0, 0)], in: o.ints(1), out: o.ints(2)}
			op.opts, op.hasOpts = o.sub(4)
			if err := sg.prepare(op); err != nil {
				return nil, err
			}
			sg.ops = append(sg.ops, op)
		}
		m.graphs = append(m.graphs, sg)
	}
	if len(m.graphs) == 0 {
		return nil, fmt.Errorf("no subgraph")
	}
	in := m.Input()
	if len(in.shape) != 3 || in.shape[2] != features || in.typ != typeInt8 {
		return nil, fmt.Errorf("model input %v is not [1, stride, %d] int8", in.shape, features)
	}
	return m, nil
}

// Input: the feature tensor [1, stride, 40] int8.
func (m *Model) Input() *tensor { return m.graphs[0].tensors[m.graphs[0].inputs[0]] }

// Stride: feature frames per inference.
func (m *Model) Stride() int { return m.Input().shape[1] }

// Invoke runs the model; returns the probability 0..255.
func (m *Model) Invoke() uint8 {
	m.names = m.names[:0]
	m.run(0)
	out := m.graphs[0].tensors[m.graphs[0].outputs[0]]
	if out.typ == typeUint8 {
		return out.u8[0]
	}
	return uint8(int32(out.i8[0]) + 128)
}

// Reset clears the streaming state (the init subgraph runs again on the next Invoke).
func (m *Model) Reset() {
	m.variables = map[string][]int8{}
	m.called = map[int]bool{}
}

// quantizeMultiplier: TFLite's QuantizeMultiplier (frexp, Q31).
func quantizeMultiplier(real float64) (int32, int) {
	if real == 0 {
		return 0, 0
	}
	q, shift := math.Frexp(real)
	fixed := int64(math.Round(q * (1 << 31)))
	if fixed == 1<<31 {
		fixed /= 2
		shift++
	}
	if shift < -31 {
		return 0, 0
	}
	return int32(fixed), shift
}

func srdhm(a, b int32) int32 { // SaturatingRoundingDoublingHighMul
	if a == math.MinInt32 && b == math.MinInt32 {
		return math.MaxInt32
	}
	ab := int64(a) * int64(b)
	nudge := int64(1 << 30)
	if ab < 0 {
		nudge = 1 - (1 << 30)
	}
	return int32((ab + nudge) / (1 << 31))
}

func rdbpot(x int32, exp int) int32 { // RoundingDivideByPOT
	mask := int32(1)<<exp - 1
	rem := x & mask
	threshold := mask >> 1
	if x < 0 {
		threshold++
	}
	r := x >> exp
	if rem > threshold {
		r++
	}
	return r
}

func mbqm(x, mult int32, shift int) int32 { // MultiplyByQuantizedMultiplier
	left, right := 0, 0
	if shift > 0 {
		left = shift
	} else {
		right = -shift
	}
	return rdbpot(srdhm(x*(1<<left), mult), right)
}

func activationRange(act int, out *tensor) (int32, int32) {
	lo, hi := int32(-128), int32(127)
	zp := out.zero()
	q := func(v float32) int32 { return zp + int32(math.Round(float64(v/out.scale[0]))) }
	switch act {
	case 1: // RELU
		lo = max(lo, q(0))
	case 2: // RELU_N1_TO_1
		lo, hi = max(lo, q(-1)), min(hi, q(1))
	case 3: // RELU6
		lo, hi = max(lo, q(0)), min(hi, q(6))
	}
	return lo, hi
}

func (sg *subgraph) prepare(op *operator) error {
	switch op.code {
	case opConv, opDepthwise:
		in, filter, out := sg.tensors[op.in[0]], sg.tensors[op.in[1]], sg.tensors[op.out[0]]
		channels := out.shape[3]
		op.mult, op.shift = make([]int32, channels), make([]int, channels)
		for c := 0; c < channels; c++ {
			fs := filter.scale[0]
			if len(filter.scale) > 1 {
				fs = filter.scale[c]
			}
			op.mult[c], op.shift[c] = quantizeMultiplier(float64(in.scale[0]) * float64(fs) / float64(out.scale[0]))
		}
		act := 0
		if op.code == opConv {
			act = int(op.opts.i8(3, 0))
		} else {
			act = int(op.opts.i8(4, 0))
		}
		op.actMin, op.actMax = activationRange(act, out)
	case opFullyConn:
		in, filter, out := sg.tensors[op.in[0]], sg.tensors[op.in[1]], sg.tensors[op.out[0]]
		product := float64(in.scale[0] * filter.scale[0]) // float product, as TFLM
		m, s := quantizeMultiplier(product / float64(out.scale[0]))
		op.mult, op.shift = []int32{m}, []int{s}
		op.actMin, op.actMax = activationRange(int(op.opts.i8(0, 0)), out)
	case opConcat, opStridedSlice, opSplitV, opReshape, opLogistic, opQuantize, opCallOnce,
		opVarHandle, opReadVariable, opAssignVariable:
	default:
		return fmt.Errorf("operator %d not supported", op.code)
	}
	return nil
}
