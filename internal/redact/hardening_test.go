package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/llmspan/llmspan/internal/redact"
)

const (
	skKey  = "sk-abcdefghijklmnopqrstuvwx"
	akia   = "AKIAIOSFODNN7EXAMPLE"
	opaque = "Zx9fQ2mK7pLw3Rt8VbN4"
	hex32  = "9f86d081884c7d659a2feaa0c55ad015"
)

func TestDefaultMasksSecretsAfterEscapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, secret string }{
		{"literal backslash-n", `{"content":"key:\n` + skKey + `"}`, skKey},
		{"literal backslash-t", `{"content":"a\t` + akia + `"}`, akia},
		{"literal backslash-r", `x\r` + skKey, skKey},
		{"real newline", "key:\n" + skKey, skKey},
		{"real tab", "key:\t" + akia, akia},
		{"escaped quotes private_key", `{"c":"{\"private_key\": \"MIIEvQIBADANBgkqhkiG9w0B\"}"}`, "MIIEvQIBADANBgkqhkiG9w0B"},
		{"escaped quotes password", `{"c":"{\"password\": \"hunter2hunter2\"}"}`, "hunter2hunter2"},
	}
	for _, tc := range cases {
		for _, p := range []redact.Profile{redact.ProfileDefault, redact.ProfileStrict} {
			if got := redact.Apply(tc.in, p); strings.Contains(got, tc.secret) {
				t.Errorf("%s/%s leaked: %q", tc.name, p, got)
			}
		}
	}
}

func TestEscapesPreservedAfterRedaction(t *testing.T) {
	t.Parallel()
	in := `{"content":"line1\nkey: ` + skKey + `\tend"}`
	got := redact.Apply(in, redact.ProfileDefault)
	if !json.Valid([]byte(got)) || !strings.Contains(got, `line1\nkey: [REDACTED]\tend`) {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultNewPatterns(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, secret string }{
		{"authorization bearer opaque", "Authorization: Bearer " + opaque, opaque},
		{"authorization basic", "authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA"},
		{"json authorization", `{"Authorization":"Bearer ` + opaque + `"}`, opaque},
		{"bare bearer", "send bearer " + opaque + " now", opaque},
		{"json password", `{"password":"s3cretPass!"}`, "s3cretPass"},
		{"json api_key", `{"api_key": "abcd1234efgh"}`, "abcd1234efgh"},
		{"env assignment", "DB_PASSWORD=Tr0ub4dor3xyz", "Tr0ub4dor3xyz"},
		{"yaml token", "auth_token: a1b2c3d4e5f6g7h8", "a1b2c3d4e5f6g7h8"},
		{"passwd quoted", `passwd = 'longpassword'`, "longpassword"},
		{"azure api-key header", "api-key: " + hex32, hex32},
		{"azure storage", "AccountKey=abcdEFGH1234abcdEFGH1234abcdEFGH1234abcd==;EndpointSuffix=x", "abcdEFGH1234abcdEFGH1234abcdEFGH1234abcd"},
		{"azure sas", "https://a.blob.core.windows.net/c?sv=1&sig=abcDEF123%2Bxyz789abcDEF123", "abcDEF123%2Bxyz789abcDEF123"},
		{"hex after keyword", "the api key is " + hex32, hex32},
		{"x-api-key", "x-api-key: AbCd1234EfGh5678", "AbCd1234EfGh5678"},
	}
	for _, tc := range cases {
		got := redact.Apply(tc.in, redact.ProfileDefault)
		if strings.Contains(got, tc.secret) {
			t.Errorf("%s leaked: %q", tc.name, got)
		}
		if !strings.Contains(got, redact.Mask) {
			t.Errorf("%s: no mask: %q", tc.name, got)
		}
	}
}

func TestDefaultNegativesProseUntouched(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"The bearer of bad news arrived late.",
		"The bearer internationalization is fine",
		"the token: expired",
		"Reset your password: see the docs.",
		"max_tokens: 4096 and \"max_tokens\": 1000",
		`{"max_tokens": 100, "temperature": 0.2}`,
		"The secret: love",
		"password policy requires rotation",
		"git commit 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b is fine",
		"This is ordinary text.\nNew line with sk- and AKIA mention.",
		"authorization is required",
		`{"content":"hello\nworld"}`,
	} {
		if got := redact.Apply(in, redact.ProfileDefault); got != in {
			t.Errorf("false positive:\n in: %q\nout: %q", in, got)
		}
	}
}

func TestStrictStricter(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"password is hunter",
		"token: expired",
		"checksum 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b here",
		"blob Ab3dEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEf end",
	} {
		if got := redact.Apply(in, redact.ProfileStrict); !strings.Contains(got, redact.Mask) {
			t.Errorf("strict missed %q -> %q", in, got)
		}
	}
}

func TestIdempotentNewPatterns(t *testing.T) {
	t.Parallel()
	in := "Authorization: Bearer " + opaque + ` {"password":"abcd1234"} api key is ` + hex32 + `\n` + skKey
	for _, p := range []redact.Profile{redact.ProfileDefault, redact.ProfileStrict} {
		once := redact.Apply(in, p)
		if twice := redact.Apply(once, p); once != twice {
			t.Errorf("%s not idempotent: %q vs %q", p, once, twice)
		}
	}
}

func TestBody(t *testing.T) {
	t.Parallel()
	fn := redact.Func(redact.ProfileDefault)
	cases := []struct{ name, in, secret string }{
		{"json newline", `{"content":"key:\n` + skKey + `"}`, skKey},
		{"json unicode escape", `{"content":"k:\u000a` + skKey + `"}`, skKey},
		{"nested escaped json", `{"content":"{\"private_key\": \"MIIEvQIBADANBgkqhkiG9w0B\"}"}`, "MIIEvQIBADANBgkqhkiG9w0B"},
		{"json key", `{"` + skKey + `":1}`, skKey},
		{"sse", "data: {\"delta\":\"x\\n" + skKey + "\"}\n\ndata: [DONE]\n", skKey},
		{"truncated json", `{"content":"key:\n` + skKey + `","more":"tru`, skKey},
		{"plain text", "hi\n" + skKey, skKey},
	}
	for _, tc := range cases {
		got := redact.Body(tc.in, fn)
		if strings.Contains(got, tc.secret) {
			t.Errorf("%s leaked: %q", tc.name, got)
		}
		// result must be storable as a JSON string value
		if _, err := json.Marshal(struct{ S string }{got}); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// valid JSON stays valid JSON; benign content and number types preserved.
	got := redact.Body(`{"a":"hi <b>\n","n":12345678901234567890,"k":"`+skKey+`"}`, fn)
	if !json.Valid([]byte(got)) || !strings.Contains(got, "12345678901234567890") || !strings.Contains(got, "<b>") {
		t.Errorf("got %q", got)
	}
	if redact.Body("", fn) != "" || redact.Body("x", nil) != "x" {
		t.Error("empty/nil handling")
	}
}

func FuzzApplyEscapes(f *testing.F) {
	for _, s := range []string{`{"c":"a\nsk-abcdefghijklmnopqrstuvwx"}`, `\\n\t\r`, "Authorization: Bearer abc123def456ghi789", `password":"x`, "\uE000\\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 16*1024 {
			t.Skip()
		}
		for _, p := range []redact.Profile{redact.ProfileDefault, redact.ProfileStrict} {
			once := redact.Apply(in, p)
			if twice := redact.Apply(once, p); once != twice {
				t.Fatalf("%s not idempotent: %q / %q", p, once, twice)
			}
		}
	})
}

func BenchmarkApplyDefault(b *testing.B) {
	in := strings.Repeat("lorem ipsum dolor sit amet token: foo password bar \\n ", 400)
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		redact.Apply(in, redact.ProfileStrict)
	}
}

// Pathological input must stay linear (RE2); guard against regressions.
func TestNoPathologicalSlowdown(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("password=", 5000) + strings.Repeat("a", 100000) + strings.Repeat(`\"token\":\"`, 3000)
	_ = redact.Apply(in, redact.ProfileStrict)
}

func BenchmarkApplyDefaultProse(b *testing.B) {
	in := strings.Repeat("Please summarize the following document about quarterly revenue and mail bob. ", 300)
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		redact.Apply(in, redact.ProfileDefault)
	}
}
