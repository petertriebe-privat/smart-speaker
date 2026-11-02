package main

import (
	"context"
	"encoding/binary"
	"log"
	"time"

	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
)

// speech24k: German text to speech in the assistant's voice as 24 kHz PCM (OpenAI's "pcm" format), for
// clips that are made once and kept (the announcements of a call on the mobile phone).
func speech24k(text string) ([]int16, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, err := openai.Speech(ctx, text, "pcm")
	if err != nil {
		return nil, err
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return pcm, nil
}

// speakerName: how this Echo shows up on a phone's Bluetooth list: device.json's name, else its
// Spotify name ("Jarvis Arbeitszimmer"), else "Jarvis <room>".
func (d *device) speakerName() string {
	s := d.cfg.Settings
	switch {
	case s.Name != "":
		return s.Name
	case s.SpotifyName != "" && (s.SpotifyName != prompts.Name() || s.Room == ""):
		return s.SpotifyName
	case s.Room != "":
		return prompts.Name() + " " + s.Room
	}
	return ""
}

// judgeCall: how far a call the assistant made for the user got (phone.Deps.Judge,
// CALL_OUTCOME_INSTRUCTIONS); "" on an error, which the phone takes as "no retry".
func judgeCall(task, transcript string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := openai.Responses(ctx, map[string]any{
		"model":             prompts.Text("DELEGATION_MODEL"),
		"instructions":      prompts.Text("CALL_OUTCOME_INSTRUCTIONS"),
		"input":             "Task: " + task + "\nTranscript:\n" + transcript,
		"store":             false,
		"max_output_tokens": 400,
		"reasoning":         map[string]any{"effort": "low"},
	})
	if err != nil {
		log.Printf("[Phone] judging the call failed: %v", err)
		return ""
	}
	return resp.Text()
}
