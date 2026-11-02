package alsa

import (
	"reflect"
	"testing"
)

func TestParseValues(t *testing.T) {
	enum := controlInfo{typ: typeEnumerated, count: 1, items: []string{"Off", "On"}}
	if v, err := parseValues("amp", enum, []string{"On"}); err != nil || !reflect.DeepEqual(v, []int32{1}) {
		t.Errorf("enum: %v %v", v, err)
	}
	if _, err := parseValues("amp", enum, []string{"on"}); err == nil {
		t.Error("enum items are case-sensitive")
	}
	stereo := controlInfo{typ: typeInteger, count: 2}
	if v, err := parseValues("gain", stereo, []string{"6"}); err != nil || !reflect.DeepEqual(v, []int32{6, 6}) {
		t.Errorf("one value for both channels: %v %v", v, err)
	}
	if v, err := parseValues("gain", stereo, []string{"3", "4"}); err != nil || !reflect.DeepEqual(v, []int32{3, 4}) {
		t.Errorf("per channel: %v %v", v, err)
	}
	if v, err := parseValues("switch", controlInfo{typ: typeBoolean, count: 2}, []string{"on"}); err != nil || !reflect.DeepEqual(v, []int32{1, 1}) {
		t.Errorf("boolean: %v %v", v, err)
	}
	if _, err := parseValues("eq", controlInfo{typ: typeBytes, count: 117}, []string{"0"}); err == nil {
		t.Error("bytes take SetBytes")
	}
}
