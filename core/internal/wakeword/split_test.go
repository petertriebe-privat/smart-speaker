package wakeword

import "testing"

// SPLIT_V along the channels of a [1, 2, 1, 5] tensor into 2 and -1 (the rest: 3), and along time.
func TestSplitV(t *testing.T) {
	in := &tensor{shape: []int{1, 2, 1, 5}, i8: []int8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
	a := &tensor{shape: []int{1, 2, 1, 2}, i8: make([]int8, 4)}
	b := &tensor{shape: []int{1, 2, 1, 3}, i8: make([]int8, 6)}
	sg := &subgraph{tensors: []*tensor{in, a, b}}
	splitV(&operator{out: []int{1, 2}}, sg, in, []int32{2, -1}, -1)
	if a.i8[0] != 1 || a.i8[1] != 2 || a.i8[2] != 6 || a.i8[3] != 7 {
		t.Errorf("first part %v", a.i8)
	}
	if b.i8[0] != 3 || b.i8[2] != 5 || b.i8[3] != 8 || b.i8[5] != 10 {
		t.Errorf("second part %v", b.i8)
	}
	// along axis 1 (time): the two rows apart
	r0 := &tensor{shape: []int{1, 1, 1, 5}, i8: make([]int8, 5)}
	r1 := &tensor{shape: []int{1, 1, 1, 5}, i8: make([]int8, 5)}
	sg = &subgraph{tensors: []*tensor{in, r0, r1}}
	splitV(&operator{out: []int{1, 2}}, sg, in, []int32{1, 1}, 1)
	if r0.i8[4] != 5 || r1.i8[0] != 6 {
		t.Errorf("rows %v %v", r0.i8, r1.i8)
	}
}
