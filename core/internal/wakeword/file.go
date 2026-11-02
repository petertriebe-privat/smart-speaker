package wakeword

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadFile reads a model (.tflite) and its microWakeWord manifest (the same name with .json, as
// hey_jarvis.json) if there is one: the model's bytes and its sliding window (0 = no manifest,
// use the default). The model is checked by loading it, so an unsupported operator fails here
// and not at the first inference. The flatbuffer reader does not check bounds: a damaged file
// panics in it, which becomes an error here (a truncated model crashed the test, 20.10.2026).
func LoadFile(path string) (model []byte, window int, err error) {
	model, err = os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if err := checkModel(model); err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(strings.TrimSuffix(path, ".tflite") + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return model, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var manifest struct {
		Micro struct {
			Window int `json:"sliding_window_size"`
		} `json:"micro"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, 0, err
	}
	return model, manifest.Micro.Window, nil
}

func checkModel(model []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("damaged model: %v", r)
		}
	}()
	m, err := LoadModel(model)
	if err == nil {
		m.Invoke() // one inference touches every operator and tensor
	}
	return err
}
