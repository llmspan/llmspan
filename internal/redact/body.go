package redact

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Body redacts a captured HTTP body with fn, handling JSON properly.
//
// Redacting the raw JSON text is unsafe: patterns are \b-anchored and a
// string value such as "key:\nsk-..." has a literal backslash-n (a word
// character) in front of the secret, while escaped quotes defeat
// key/value patterns. So:
//
//   - valid JSON: every string value (and object key) is decoded, passed
//     through fn, and the document re-marshalled;
//   - SSE streams: each `data:` line holding valid JSON is treated as
//     above, other lines go through fn as text;
//   - anything else (truncated bodies, plain text): fn runs on the text.
//     Apply/Func are escape-aware so this is still safe for JSON-ish
//     fragments.
//
// The result is always a valid Go string to embed in a JSON field.
func Body(raw string, fn func(string) string) string {
	if raw == "" || fn == nil {
		return raw
	}
	if out, ok := jsonDoc(raw, fn); ok {
		return out
	}
	if strings.Contains(raw, "data:") && strings.Contains(raw, "\n") {
		lines := strings.Split(raw, "\n")
		for i, line := range lines {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				if out, ok := jsonDoc(strings.TrimSpace(rest), fn); ok {
					lines[i] = "data: " + out
					continue
				}
			}
			lines[i] = fn(line)
		}
		return strings.Join(lines, "\n")
	}
	return fn(raw)
}

func jsonDoc(raw string, fn func(string) string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[' && trimmed[0] != '"') {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return "", false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(walk(v, fn)); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

func walk(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case []any:
		for i := range t {
			t[i] = walk(t[i], fn)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv := walk(val, fn)
			if sv, ok := val.(string); ok {
				// Key context: "password":"x" only matches as a pair.
				pair := `"` + k + `":"` + sv + `"`
				if fn(pair) != `"`+fn(k)+`":"`+fn(sv)+`"` {
					nv = Mask
				}
			}
			out[fn(k)] = nv
		}
		return out
	default:
		return v
	}
}
