package audio

import "testing"

func TestBTMusicLevel(t *testing.T) {
	b := NewBTMusic(40, 20)
	// 0.4² / 1.18 = 13.6 %: as loud as the assistant's own music at 40, and audiod's gain does not clip
	if got := b.percent(); got != 14 {
		t.Errorf("40: %d %%", got)
	}
	b.Duck(true)
	if got := b.percent(); got != 3 {
		t.Errorf("ducked: %d %%", got)
	}
	b.Duck(false)
	b.SetVolume(100)
	if got := b.percent(); got != 85 { // never 100 %: 1.18 times that clipped
		t.Errorf("100: %d %%", got)
	}
}
