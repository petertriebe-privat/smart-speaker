package files

import (
	"strings"
	"testing"

	"echo-assistant/internal/google"
	"echo-assistant/internal/tools"
)

func TestMimeOf(t *testing.T) {
	for name, want := range map[string]string{"A.CSV": "text/csv", "x.pdf": "application/pdf", "x.JPEG": "image/jpeg", "x.bin": "application/octet-stream"} {
		if got := MimeOf(name); got != want {
			t.Errorf("MimeOf(%q) = %q", name, got)
		}
	}
}

func TestCleanName(t *testing.T) {
	if got := cleanName(" a/b\\c:d\".txt\n"); got != "a_b_c_d_.txt" {
		t.Errorf("got %q", got)
	}
	if got := cleanName(""); got != "datei.txt" {
		t.Errorf("got %q", got)
	}
	if got := cleanName(strings.Repeat("ä", 50)); len(got) > 60 || got == "" {
		t.Errorf("long name: %q", got)
	}
}

func TestFind(t *testing.T) {
	list := []google.Entry{{Name: "Rechnung-Mai.pdf"}, {Name: "rechnung.pdf"}, {Name: "Foto.jpg"}}
	if find(list, "RECHNUNG.PDF") != 1 {
		t.Error("exact match, ignoring case")
	}
	if find(list, "mai") != 0 {
		t.Error("partial match")
	}
	if find(list, "zzz") != -1 || find(list, "") != -1 {
		t.Error("no match")
	}
}

func TestSendFileErrors(t *testing.T) {
	s := New(nil, nil)
	var r tools.Registry
	s.AddTools(&r)
	out := r.Call("send_file", `{"file":"","name":"a.txt","content":"x","via":"carrier pigeon","to":"","text":""}`, tools.Context{})
	if out != "Error: via is telegram, email or print." {
		t.Error(out)
	}
	if out := r.Call("list_files", `{}`, tools.Context{}); !strings.Contains(out, "No Google Drive") {
		t.Error(out)
	}
	if out := r.Call("send_file", `{"file":"x","name":"","content":"","via":"print","to":"","text":""}`, tools.Context{}); !strings.HasPrefix(out, "Error: no Google Drive") {
		t.Error(out)
	}
}
