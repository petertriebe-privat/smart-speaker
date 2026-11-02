package wakeword

import (
	"math"
	"math/rand"
	"testing"
)

// TestLayersAgainstFloat runs the model on random inputs and checks every conv / depthwise / FC
// output against the same layer computed in float from its int8 inputs: a correct requantization
// stays within 1 LSB.
func TestLayersAgainstFloat(t *testing.T) {
	m, err := LoadModel(builtin)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	in := m.Input()
	sg := m.graphs[0]
	worst := map[int]float64{}
	for n := 0; n < 50; n++ {
		for i := range in.i8 {
			in.i8[i] = int8(rng.Intn(120) - 128)
		}
		m.Invoke()
		for oi, op := range sg.ops {
			if op.code != opConv && op.code != opDepthwise && op.code != opFullyConn {
				continue
			}
			x, w, b, out := sg.tensors[op.in[0]], sg.tensors[op.in[1]], sg.tensors[op.in[2]], sg.tensors[op.out[0]]
			deq := func(t *tensor, i, ch int) float64 {
				s := t.scale[0]
				if len(t.scale) > 1 {
					s = t.scale[ch]
				}
				return float64(s) * float64(int32(t.i8[i])-t.zero())
			}
			outC := out.shape[len(out.shape)-1]
			for oc := 0; oc < outC; oc++ {
				var acc float64
				switch op.code {
				case opFullyConn:
					depth := w.shape[1]
					for d := 0; d < depth; d++ {
						acc += deq(x, d, 0) * deq(w, oc*depth+d, 0)
					}
				case opConv:
					// only the shapes of this model: output 1x1
					fH, inC := w.shape[1], w.shape[3]
					stride := int(op.opts.i32(2, 1))
					_ = stride
					if fH == 1 {
						for ic := 0; ic < inC; ic++ {
							acc += deq(x, ic, 0) * deq(w, oc*inC+ic, oc)
						}
					} else {
						for fy := 0; fy < fH; fy++ {
							for ic := 0; ic < inC; ic++ {
								acc += deq(x, fy*inC+ic, 0) * deq(w, (oc*fH+fy)*inC+ic, oc)
							}
						}
					}
				case opDepthwise:
					fH := w.shape[1]
					for fy := 0; fy < fH; fy++ {
						acc += deq(x, fy*outC+oc, 0) * deq(w, fy*outC+oc, oc)
					}
				}
				bs := float64(x.scale[0]) * float64(w.scale[0])
				if len(w.scale) > 1 {
					bs = float64(x.scale[0]) * float64(w.scale[oc])
				}
				acc += float64(b.i32[oc]) * bs
				q := math.Round(acc/float64(out.scale[0])) + float64(out.zero())
				q = math.Max(float64(op.actMin), math.Min(float64(op.actMax), q))
				d := math.Abs(q - float64(out.i8[oc]))
				if d > worst[oi] {
					worst[oi] = d
				}
			}
		}
	}
	for oi, d := range worst {
		if d > 1 {
			t.Errorf("op %d (code %d): off by %v LSB", oi, sg.ops[oi].code, d)
		}
	}
	t.Logf("worst per op: %v", worst)
}
