package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmspan/llmspan/internal/redact"
)

func TestRecorderRedactsAndLoads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "recording.jsonl")
	rec := NewRecorder(path, func(s string) string {
		return strings.ReplaceAll(s, "secret", "[REDACTED]")
	})
	if err := rec.Append(Entry{
		Method: "POST", Path: "/v1/responses?key=secret",
		Request: `{"token":"secret"}`, Response: `{"text":"secret"}`,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	got := entries[0]
	if strings.Contains(got.Path+got.Request+got.Response, "secret") {
		t.Fatalf("recording leaked secret: %+v", got)
	}
}

func TestDiffCanonicalJSON(t *testing.T) {
	t.Parallel()
	left := []Entry{{Status: 200, Response: `{"a":1,"b":2}`}}
	right := []Entry{{Status: 200, Response: `{ "b": 2, "a": 1 }`}}
	if got := Diff(left, right); len(got) != 0 {
		t.Fatalf("diff = %+v", got)
	}
	right[0].Response = `{"a":2}`
	if got := Diff(left, right); len(got) != 1 || got[0].Field != "response" {
		t.Fatalf("diff = %+v", got)
	}
}

func TestRecorderRedactsEscapedSecrets(t *testing.T) {
	t.Parallel()
	fn := redact.Func(redact.ProfileDefault)
	const sk = "sk-abcdefghijklmnopqrstuvwx"
	cases := []struct{ name, req, resp, secret string }{
		{"newline", `{"content":"key:\n` + sk + `"}`, "", sk},
		{"tab akia", `{"content":"x\tAKIAIOSFODNN7EXAMPLE"}`, "", "AKIAIOSFODNN7EXAMPLE"},
		{"escaped private_key", `{"content":"{\"private_key\": \"MIIEvQIBADANBgkqhkiG9w0B\"}"}`, "", "MIIEvQIBADANBgkqhkiG9w0B"},
		{"sse response", "", "data: {\"d\":\"a\\n" + sk + "\"}\n\ndata: [DONE]\n", sk},
		{"truncated response", "", `{"d":"a\n` + sk + `","x":"tr`, sk},
		{"bearer", `{"h":"Authorization: Bearer Zx9fQ2mK7pLw3Rt8VbN4"}`, "", "Zx9fQ2mK7pLw3Rt8VbN4"},
		{"password", `{"password":"hunter2hunter2"}`, "", "hunter2hunter2"},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "r.jsonl")
		rec := NewRecorder(path, fn)
		if err := rec.Append(Entry{Method: "POST", Path: "/x", Request: tc.req, Response: tc.resp}); err != nil {
			t.Fatal(tc.name, err)
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), tc.secret) {
			t.Errorf("%s leaked: %s", tc.name, raw)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("%s: unloadable: %v", tc.name, err)
		}
	}
}

func TestRecorderBenignJSONUnchangedSemantically(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	rec := NewRecorder(path, redact.Func(redact.ProfileDefault))
	in := `{"messages":[{"role":"user","content":"hello\nworld, the token: expired"}]}`
	if err := rec.Append(Entry{Method: "POST", Path: "/x", Request: in}); err != nil {
		t.Fatal(err)
	}
	es, err := Load(path)
	if err != nil || len(es) != 1 {
		t.Fatal(err)
	}
	if got := canonical(es[0].Request); string(got) != string(canonical(in)) {
		t.Fatalf("got %s", got)
	}
}

func TestRecorderRefusesSymlinkAndMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	link := filepath.Join(dir, "rec.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	if err := NewRecorder(link, nil).Append(Entry{Method: "GET", Path: "/"}); err == nil {
		t.Fatal("expected symlink to be refused")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("symlink target was created")
	}
	path := filepath.Join(dir, "ok.jsonl")
	if err := NewRecorder(path, nil).Append(Entry{Method: "GET", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestRecorderSizeCap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	rec := NewRecorder(path, nil)
	rec.MaxBytes = 600
	for i := 0; i < 20; i++ {
		if err := rec.Append(Entry{Method: "GET", Path: "/x", Request: strings.Repeat("a", 100)}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > 600 || st.Size() == 0 {
		t.Fatalf("size = %v err=%v", st, err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
