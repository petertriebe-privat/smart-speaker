package wakeword

import "math"

func clamp(v, lo, hi int32) int32 { return max(lo, min(hi, v)) }

func (m *Model) run(g int) {
	sg := m.graphs[g]
	for _, op := range sg.ops {
		t := func(i int) *tensor { return sg.tensors[op.in[i]] }
		var out *tensor
		if len(op.out) > 0 {
			out = sg.tensors[op.out[0]]
		}
		switch op.code {
		case opCallOnce:
			sub := int(op.opts.i32(0, 0))
			if !m.called[sub] {
				m.called[sub] = true
				m.run(sub)
			}
		case opVarHandle:
			out.res = len(m.names)
			m.names = append(m.names, op.opts.str(1))
		case opReadVariable:
			copy(out.i8, m.variables[m.names[t(0).res]])
		case opAssignVariable:
			name := m.names[t(0).res]
			v := m.variables[name]
			if len(v) != len(t(1).i8) {
				v = make([]int8, len(t(1).i8))
				m.variables[name] = v
			}
			copy(v, t(1).i8)
		case opReshape:
			copy(out.i8, t(0).i8)
		case opConcat:
			concat(op, sg, out)
		case opStridedSlice:
			stridedSlice(op, t(0), t(1).i32, t(2).i32, t(3).i32, out)
		case opSplitV:
			splitV(op, sg, t(0), t(1).i32, int(t(2).i32[0]))
		case opConv:
			conv(op, t(0), t(1), t(2), out)
		case opDepthwise:
			depthwise(op, t(0), t(1), t(2), out)
		case opFullyConn:
			fullyConnected(op, t(0), t(1), t(2), out)
		case opLogistic:
			in := t(0)
			for i, v := range in.i8 {
				x := float64(in.scale[0]) * float64(int32(v)-in.zero())
				y := 1 / (1 + math.Exp(-x))
				out.i8[i] = int8(clamp(int32(math.Round(y/float64(out.scale[0])))+out.zero(), -128, 127))
			}
		case opQuantize:
			in := t(0)
			for i, v := range in.i8 {
				x := float64(in.scale[0]) * float64(int32(v)-in.zero())
				q := int32(math.Round(x/float64(out.scale[0]))) + out.zero()
				if out.typ == typeUint8 {
					out.u8[i] = uint8(clamp(q, 0, 255))
				} else {
					out.i8[i] = int8(clamp(q, -128, 127))
				}
			}
		}
	}
}

// concat along op's axis; all inputs share the output's quantization in these models (checked:
// otherwise requantized).
func concat(op *operator, sg *subgraph, out *tensor) {
	axis := int(op.opts.i32(0, 0))
	if axis < 0 {
		axis += len(out.shape)
	}
	outer, inner := 1, 1
	for i := 0; i < axis; i++ {
		outer *= out.shape[i]
	}
	for i := axis + 1; i < len(out.shape); i++ {
		inner *= out.shape[i]
	}
	pos := 0
	for o := 0; o < outer; o++ {
		for _, idx := range op.in {
			in := sg.tensors[idx]
			n := in.shape[axis] * inner
			src := in.i8[o*n : (o+1)*n]
			same := in.scale[0] == out.scale[0] && in.zero() == out.zero()
			for i, v := range src {
				if same {
					out.i8[pos+i] = v
				} else {
					x := float64(in.scale[0]) * float64(int32(v)-in.zero())
					out.i8[pos+i] = int8(clamp(int32(math.Round(x/float64(out.scale[0])))+out.zero(), -128, 127))
				}
			}
			pos += n
		}
	}
}

// splitV cuts in along axis into the outputs (sizes as given, one -1 takes the rest); the outputs
// share the input's quantization (TFLite requires it), so it is a copy. microWakeWord's mixnet
// blocks with several kernel sizes use it since 2025 (the stock hey_jarvis of 2024 did not).
func splitV(op *operator, sg *subgraph, in *tensor, sizes []int32, axis int) {
	if axis < 0 {
		axis += len(in.shape)
	}
	outer, inner := 1, 1
	for i := 0; i < axis; i++ {
		outer *= in.shape[i]
	}
	for i := axis + 1; i < len(in.shape); i++ {
		inner *= in.shape[i]
	}
	dim := in.shape[axis]
	n := make([]int, len(op.out))
	rest, open := dim, -1
	for k := range n {
		if k < len(sizes) && sizes[k] >= 0 {
			n[k] = int(sizes[k])
			rest -= n[k]
		} else {
			open = k
		}
	}
	if open >= 0 {
		n[open] = rest
	}
	for o := 0; o < outer; o++ {
		pos := o * dim * inner
		for k, idx := range op.out {
			out := sg.tensors[idx]
			w := n[k] * inner
			copy(out.i8[o*w:(o+1)*w], in.i8[pos:pos+w])
			pos += w
		}
	}
}

// stridedSlice for 4-D tensors with positive strides (begin/end masks; no ellipsis, new or
// shrink axes in these models).
func stridedSlice(op *operator, in *tensor, begin, end, strides []int32, out *tensor) {
	beginMask, endMask := op.opts.i32(0, 0), op.opts.i32(1, 0)
	var b, e, s [4]int
	for d := 0; d < 4; d++ {
		dim := in.shape[d]
		s[d] = int(strides[d])
		b[d], e[d] = int(begin[d]), int(end[d])
		if beginMask&(1<<d) != 0 {
			b[d] = 0
		} else if b[d] < 0 {
			b[d] += dim
		}
		if endMask&(1<<d) != 0 {
			e[d] = dim
		} else if e[d] < 0 {
			e[d] += dim
		}
		b[d], e[d] = max(0, min(dim, b[d])), max(0, min(dim, e[d]))
	}
	sh := in.shape
	pos := 0
	for i0 := b[0]; i0 < e[0]; i0 += s[0] {
		for i1 := b[1]; i1 < e[1]; i1 += s[1] {
			for i2 := b[2]; i2 < e[2]; i2 += s[2] {
				for i3 := b[3]; i3 < e[3]; i3 += s[3] {
					out.i8[pos] = in.i8[((i0*sh[1]+i1)*sh[2]+i2)*sh[3]+i3]
					pos++
				}
			}
		}
	}
}

// padding: TFLite's ComputePaddingHeightWidth for one dimension.
func padding(same bool, stride, in, filter, out int) int {
	if !same {
		return 0
	}
	return max(0, ((out-1)*stride+filter-in)/2)
}

func conv(op *operator, in, filter, bias, out *tensor) {
	o := op.opts
	same := o.i8(0, 0) == 0
	strideW, strideH := int(o.i32(1, 1)), int(o.i32(2, 1))
	dilW, dilH := int(o.i32(4, 1)), int(o.i32(5, 1))
	inH, inW, inC := in.shape[1], in.shape[2], in.shape[3]
	outH, outW, outC := out.shape[1], out.shape[2], out.shape[3]
	fH, fW := filter.shape[1], filter.shape[2]
	padH := padding(same, strideH, inH, (fH-1)*dilH+1, outH)
	padW := padding(same, strideW, inW, (fW-1)*dilW+1, outW)
	inOff := -in.zero()
	for oy := 0; oy < outH; oy++ {
		for ox := 0; ox < outW; ox++ {
			for oc := 0; oc < outC; oc++ {
				var acc int32
				for fy := 0; fy < fH; fy++ {
					iy := oy*strideH - padH + dilH*fy
					if iy < 0 || iy >= inH {
						continue
					}
					for fx := 0; fx < fW; fx++ {
						ix := ox*strideW - padW + dilW*fx
						if ix < 0 || ix >= inW {
							continue
						}
						ib := (iy*inW + ix) * inC
						fb := ((oc*fH+fy)*fW + fx) * inC
						for ic := 0; ic < inC; ic++ {
							acc += int32(filter.i8[fb+ic]) * (int32(in.i8[ib+ic]) + inOff)
						}
					}
				}
				if bias != nil && len(bias.i32) > 0 {
					acc += bias.i32[oc]
				}
				acc = mbqm(acc, op.mult[oc], op.shift[oc]) + out.zero()
				out.i8[(oy*outW+ox)*outC+oc] = int8(clamp(acc, op.actMin, op.actMax))
			}
		}
	}
}

func depthwise(op *operator, in, filter, bias, out *tensor) {
	o := op.opts
	same := o.i8(0, 0) == 0
	strideW, strideH := int(o.i32(1, 1)), int(o.i32(2, 1))
	mult := int(o.i32(3, 1))
	dilW, dilH := int(o.i32(5, 1)), int(o.i32(6, 1))
	inH, inW, inC := in.shape[1], in.shape[2], in.shape[3]
	outH, outW, outC := out.shape[1], out.shape[2], out.shape[3]
	fH, fW := filter.shape[1], filter.shape[2]
	padH := padding(same, strideH, inH, (fH-1)*dilH+1, outH)
	padW := padding(same, strideW, inW, (fW-1)*dilW+1, outW)
	inOff := -in.zero()
	for oy := 0; oy < outH; oy++ {
		for ox := 0; ox < outW; ox++ {
			for ic := 0; ic < inC; ic++ {
				for mi := 0; mi < mult; mi++ {
					oc := ic*mult + mi
					var acc int32
					for fy := 0; fy < fH; fy++ {
						iy := oy*strideH - padH + dilH*fy
						if iy < 0 || iy >= inH {
							continue
						}
						for fx := 0; fx < fW; fx++ {
							ix := ox*strideW - padW + dilW*fx
							if ix < 0 || ix >= inW {
								continue
							}
							acc += int32(filter.i8[(fy*fW+fx)*outC+oc]) * (int32(in.i8[(iy*inW+ix)*inC+ic]) + inOff)
						}
					}
					if bias != nil && len(bias.i32) > 0 {
						acc += bias.i32[oc]
					}
					acc = mbqm(acc, op.mult[oc], op.shift[oc]) + out.zero()
					out.i8[(oy*outW+ox)*outC+oc] = int8(clamp(acc, op.actMin, op.actMax))
				}
			}
		}
	}
}

func fullyConnected(op *operator, in, filter, bias, out *tensor) {
	depth := filter.shape[1]
	units := filter.shape[0]
	batches := len(in.i8) / depth
	inOff, fOff := -in.zero(), -filter.zero()
	for b := 0; b < batches; b++ {
		for u := 0; u < units; u++ {
			var acc int32
			for d := 0; d < depth; d++ {
				acc += (int32(in.i8[b*depth+d]) + inOff) * (int32(filter.i8[u*depth+d]) + fOff)
			}
			if bias != nil && len(bias.i32) > 0 {
				acc += bias.i32[u]
			}
			acc = mbqm(acc, op.mult[0], op.shift[0]) + out.zero()
			out.i8[b*units+u] = int8(clamp(acc, op.actMin, op.actMax))
		}
	}
}
