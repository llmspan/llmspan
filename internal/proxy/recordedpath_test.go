package proxy

import (
	"net/url"
	"testing"
)

func TestRecordedPathDropsCredentials(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/g:generateContent?key=AIzaSECRET":           "/v1beta/models/g:generateContent",
		"/v1beta/models/g:streamGenerateContent?alt=sse&key=SECRET": "/v1beta/models/g:streamGenerateContent?alt=sse",
		"/v1/chat/completions":                                      "/v1/chat/completions",
	}
	for in, want := range cases {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := recordedPath(u); got != want {
			t.Errorf("recordedPath(%q) = %q, want %q", in, got, want)
		}
	}
}
