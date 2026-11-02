package firetv

import "testing"

func TestParseState(t *testing.T) {
	out := "mWakefulness=Awake\n" +
		"mCurrentFocus=Window{1a2b u0 com.netflix.ninja/com.netflix.Main}\n" +
		"package=com.netflix.ninja\n" +
		"state=PlaybackState {state=3, position=1234, buffered position=0}\n" +
		"  cmp=android/com.android.internal.app.ResolverActivity dat=https://www.disneyplus.com/x flg=0x1\n"
	st := parseState(out)
	if st.power != "on" || st.front != "com.netflix.ninja" || st.playing != "com.netflix.ninja" ||
		st.paused || st.position != 1234 || st.chooser != "https://www.disneyplus.com/x" {
		t.Fatalf("unexpected state %+v", st)
	}
	if got := describe(st); got != `on, in front: Netflix, playing: Netflix, an "Open with" dialog waits` {
		t.Fatalf("describe: %q", got)
	}
}

func TestFindApp(t *testing.T) {
	list := "com.netflix.ninja\ncom.disney.disneyplus\ncom.other.app\n"
	if pkg, _ := findApp(list, "netflix"); pkg != "com.netflix.ninja" {
		t.Fatalf("netflix: %q", pkg)
	}
	if pkg, msg := findApp(list, "com."); pkg != "" || msg == "" {
		t.Fatalf("ambiguous: %q %q", pkg, msg)
	}
}

func TestFind(t *testing.T) {
	tvs := []tv{{Name: "Fire TV A", Area: "Room A"}, {Name: "Fire TV B", Area: "Room B"}}
	if got, _ := find(tvs, "room b"); got == nil || got.Name != "Fire TV B" {
		t.Fatalf("room: %+v", got)
	}
	if got, msg := find(tvs, "fire"); got != nil || msg == "" {
		t.Fatalf("several: %+v %q", got, msg)
	}
}
