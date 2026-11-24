// Package redact masks sensitive substrings inside captured prompt and
// completion content before that content reaches a span attribute or
// the otelslog bridge.
//
// llmspan's privacy story has long been "content capture is off by
// default; pair it with collector-side redaction when you turn it on".
// That is not a credible offer to operators whose first deployment is
// a few servers behind one collector — they can opt into events for
// debugging, only to discover six weeks later that an `sk-...` ended
// up in a prompt the model ignored. The proxy is the right place to
// scrub: it sees content first and has bounded surface to test.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// Profile names the redaction policy applied to captured content.
type Profile string

const (
	// ProfileOff disables redaction. Content reaches span attributes
	// raw — for operators who explicitly want unfiltered visibility
	// and accept the privacy cost.
	ProfileOff Profile = "off"

	// ProfileDefault is the recommended policy. Masks high-confidence
	// credential and PII patterns that show up in real LLM traffic:
	// API key prefixes (OpenAI sk-, Slack xox*, AWS AKIA), GCP
	// service-account JSON markers, common JWT shape, RFC-5322
	// emails.
	ProfileDefault Profile = "default"

	// ProfileStrict adds higher-false-positive PII patterns on top
	// of ProfileDefault: credit-card Luhn-shaped sequences, US SSN,
	// E.164 phone numbers. For regulated workloads.
	ProfileStrict Profile = "strict"
)

// Mask is the replacement string used in place of detected secrets.
// Kept short so spans aren't padded; "[REDACTED]" is recognizable
// when scrolling a trace.
const Mask = "[REDACTED]"

// Apply replaces every match of the profile's pattern set with Mask.
// Unknown profiles fall back to identity (no-op) — callers should
// validate at config time and never reach the default arm here.
//
// Text that still carries literal JSON escape sequences (a backslash
// followed by n, t or r — the shape of a raw JSON body, an SSE chunk or a
// truncated body) is handled by temporarily mapping those sequences to
// non-word placeholder runes, so that `\b`-anchored patterns still see a
// boundary in "key:\nsk-..." and the original escapes are restored
// afterwards (the output stays valid inside a JSON string).
func Apply(s string, profile Profile) string {
	switch profile {
	case ProfileOff, "":
		return s
	case ProfileDefault, ProfileStrict:
	default:
		return s
	}
	if s == "" {
		return s
	}
	s, restore := shieldEscapes(s)
	s = applyAll(s, defaultPatterns)
	if profile == ProfileStrict {
		s = applyAll(s, strictPatterns)
	}
	if restore {
		s = unshieldEscapes(s)
	}
	return s
}

// Placeholder runes (Unicode private use) standing in for literal
// backslash-n / -t / -r while patterns run.
const (
	phNL  = '\uE000'
	phTAB = '\uE001'
	phCR  = '\uE002'
)

func shieldEscapes(s string) (string, bool) {
	if !strings.ContainsRune(s, '\\') && !strings.ContainsAny(s, "\uE000\uE001\uE002") {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteRune(phNL)
				changed = true
			case 't':
				b.WriteRune(phTAB)
				changed = true
			case 'r':
				b.WriteRune(phCR)
				changed = true
			default:
				b.WriteByte(c)
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()
	// Input that already contains our placeholders would be rewritten on
	// restore; neutralise them.
	if strings.ContainsAny(s, "\uE000\uE001\uE002") {
		out = strings.Map(func(r rune) rune {
			if r == phNL || r == phTAB || r == phCR {
				return '\uFFFD'
			}
			return r
		}, s)
		return shieldEscapes(out)
	}
	return out, changed
}

func unshieldEscapes(s string) string {
	return strings.NewReplacer(string(phNL), `\n`, string(phTAB), `\t`, string(phCR), `\r`).Replace(s)
}

// Func returns a redaction closure bound to a profile. Use it when
// passing the redactor across an API boundary that wants a `func(string)
// string` shape instead of (string, Profile) → string.
//
// Returns nil for ProfileOff so callers can cheaply skip work via a
// nil check; non-nil for every other profile, so the gate is "if r !=
// nil { s = r(s) }".
func Func(profile Profile) func(string) string {
	if profile == ProfileOff || profile == "" {
		return nil
	}
	return func(s string) string { return Apply(s, profile) }
}

// Valid reports whether p is one of the known profiles. Config
// validation should refuse unknown values rather than degrade.
func Valid(p Profile) bool {
	switch p {
	case ProfileOff, ProfileDefault, ProfileStrict, "":
		return true
	}
	return false
}

// replacer is the shape applyAll consumes. *regexp.Regexp satisfies
// it natively; *luhnReplacer wraps a regexp with a Luhn check.
type replacer interface {
	ReplaceAllString(s, repl string) string
}

// applyAll runs each pattern against s and replaces every match with
// Mask. Patterns are anchored loosely on purpose — they look for the
// distinguishing prefix or character set, not the whole structural
// envelope. False negatives are preferred over false positives that
// would mangle prompt text.
func applyAll(s string, pats []replacer) string {
	lower := strings.ToLower(s)
	for _, p := range pats {
		if h, ok := p.(*keepReplacer); ok && len(h.hints) > 0 && !containsAny(lower, h.hints) {
			continue // cheap prefilter: skip regex when no keyword is present
		}
		s = p.ReplaceAllString(s, Mask)
	}
	return s
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

var (
	authHints   = []string{"authorization", "bearer"}
	azureHints  = []string{"accountkey", "sharedaccess", "subscription-key", "api-key", "sig="}
	secretHints = []string{"key", "secret", "token", "passw", "pwd", "passphrase"}
)

var (
	// defaultPatterns: high-confidence credential / PII markers.
	// Provider coverage is best-effort: we catch every shape that has
	// a distinguishing prefix or fixed-width body. Providers whose
	// tokens are bare hex (Mistral, Together, Cohere) can't be matched
	// without false-positive-bombing arbitrary hex strings in prose —
	// those operators should pair `redact: default` with a custom
	// collector-side scrubber.
	defaultPatterns = []replacer{
		// OpenAI / Anthropic / DeepSeek "sk-" keys (Stripe uses sk_live_/sk_test_, not matched here): at least
		// 20 chars of [A-Za-z0-9_-] after the prefix. The 20-char
		// floor cuts down on false positives from prose like
		// "sk-and-flowers". Anthropic's `sk-ant-api03-…` matches this
		// pattern as a side effect (the body is longer than 20 chars).
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		// Slack bot / user / app / refresh / svc tokens.
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
		// AWS access key ID.
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		// Google AI Studio / Cloud API keys: literal `AIza` prefix
		// followed by exactly 35 chars of [A-Za-z0-9_-]. The fixed
		// length is the right anchor — Google's keys are uniform 39
		// chars total.
		regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{35}\b`),
		// Groq API keys: `gsk_` prefix + alphanumeric body. The
		// public docs show 52-char bodies today, but the prefix is
		// distinctive enough on its own — use {20,} so the pattern
		// survives Groq tweaking the body length.
		regexp.MustCompile(`\bgsk_[A-Za-z0-9]{20,}\b`),
		// Replicate API tokens: `r8_` prefix + 20+ chars. Lower
		// bound is conservative; real tokens are longer.
		regexp.MustCompile(`\br8_[A-Za-z0-9]{20,}\b`),
		// Hugging Face user access tokens: `hf_` prefix. HF traffic
		// shows up constantly in LLM prompts (model cards, curl
		// examples pasted into chats).
		regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}\b`),
		// xAI API keys: `xai-` prefix + long body.
		regexp.MustCompile(`\bxai-[A-Za-z0-9_-]{20,}\b`),
		// Raw PEM private-key blocks (RSA / EC / OPENSSH / PKCS#8 /
		// encrypted). Multi-line by construction, so (?s) lets `.`
		// cross newlines; the lazy body stops at the first END marker.
		// Public material (CERTIFICATE, PUBLIC KEY) is deliberately
		// not matched — it isn't secret and masking it would mangle
		// legitimate cert-debugging prompts.
		regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		// GitHub classic / app tokens. The `gh_` family share a
		// 36-char floor; loose upper bound covers forward growth.
		regexp.MustCompile(`\bghp_[A-Za-z0-9]{36,255}\b`),
		regexp.MustCompile(`\bgho_[A-Za-z0-9]{36,255}\b`),
		regexp.MustCompile(`\bghu_[A-Za-z0-9]{36,255}\b`),
		regexp.MustCompile(`\bghs_[A-Za-z0-9]{36,255}\b`),
		regexp.MustCompile(`\bghr_[A-Za-z0-9]{36,255}\b`),
		// GitHub fine-grained PATs are variable-length (~80-90 chars
		// in 2026); the underscore in the body class is required
		// because some fine-grained tokens contain `_`.
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,255}\b`),
		// GCP service-account JSON: the "private_key" field is a
		// fingerprint. Match conservatively on the JSON key shape.
		regexp.MustCompile(`"private_key"\s*:\s*"[^"]+"`),
		// Same field when the surrounding JSON is itself escaped inside a
		// string (`\"private_key\": \"MII...\"`).
		regexp.MustCompile(`\\"private_key\\"\s*:\s*\\"[^"\\]*(?:\\[^"][^"\\]*)*\\"`),
		// Authorization header / value: scheme + opaque credential.
		&keepReplacer{re: regexp.MustCompile(`(?i)(\bauthorization\\?"?\s*[:=]\s*\\?"?\s*(?:bearer|basic|token|apikey|digest)\s+)[A-Za-z0-9._~+/=%-]{8,}`), hints: authHints},
		// Bare "Bearer <opaque>" anywhere; must look like a credential
		// (digit or punctuation), so prose like "bearer of bad news" stays.
		&keepReplacer{re: regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{16,})`), group: 2, accept: credentialLike, hints: []string{"bearer"}},
		// Azure: storage / service-bus connection strings, APIM and
		// Azure OpenAI api-key headers, SAS signatures.
		&keepReplacer{re: regexp.MustCompile(`(?i)(\b(?:AccountKey|SharedAccessKey|Ocp-Apim-Subscription-Key|api-key|SharedAccessSignature)\\?"?\s*[=:]\s*\\?"?)[A-Za-z0-9+/=%_-]{16,}`), hints: azureHints},
		&keepReplacer{re: regexp.MustCompile(`([?&;]sig=)[A-Za-z0-9%+/=]{20,}`), hints: azureHints},
		// Quoted assignment of a secret-named key: JSON "password":"x",
		// YAML/env password="x", also with escaped quotes.
		&keepReplacer{re: quotedAssign, group: 2, hints: secretHints},
		// Unquoted `api_key = value` / `TOKEN: value`: value must look
		// like a credential (letter+digit mix or punctuation, >= 8 chars)
		// so "the token: expired" is untouched.
		&keepReplacer{re: unquotedAssign, group: 2, accept: mixedClasses(2, 8), hints: secretHints},
		// Keyword followed by a bare long hex / base64 blob:
		// "the api key is 9f86d081884c7d65...".
		&keepReplacer{re: regexp.MustCompile(`(?i)(\b(?:api[ _-]?key|secret|token|password|passcode)(?:\s+is)?[\s:=]{1,4}["']?)([A-Fa-f0-9]{32,}|[A-Za-z0-9+/_-]{40,}={0,2})`), group: 2, hints: secretHints},
		// Common JWT: three base64url segments separated by dots.
		// Length floor avoids matching random "a.b.c" substrings.
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		// RFC-5322-ish email. Strict spec is overkill for redaction;
		// this catches the common cases without trying to parse the
		// long tail.
		regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	}

	// strictPatterns: high-false-positive PII. Layered on top of
	// defaultPatterns only when explicitly requested.
	strictPatterns = []replacer{
		// Credit-card-shaped (13-19 digits, dashes / spaces allowed).
		// Luhn check is applied post-match; non-Luhn-valid hits stay
		// in the text.
		luhnSafeCC,
		// US SSN.
		regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
		// Any value assigned to a secret-named key, even short / plain.
		&keepReplacer{re: unquotedAssign, group: 2, accept: func(v string) bool { return len(v) >= 6 }, hints: secretHints},
		&keepReplacer{re: regexp.MustCompile(`(?i)(\b(?:key|secret|token|password|passwd|pwd)\b\s*(?:is|=|:)\s*["']?)([^\s"',;]{6,})`), group: 2, hints: secretHints},
		// Bare long hex digests (hash-like secrets) and mixed-class
		// 40+ char blobs. Will also hit git SHAs and checksums.
		regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
		&keepReplacer{re: regexp.MustCompile(`()\b[A-Za-z0-9+/_-]{40,}={0,2}`), accept: mixedClasses(3, 40)},
		// E.164 phone — +<country><10–15 digits>.
		regexp.MustCompile(`\+\d{8,15}\b`),
	}
)

const secretKeyWords = `(?:api[_-]?key|apikey|secret|client[_-]?secret|token|access[_-]?key|passw(?:or)?d|passwd|pwd|passphrase)`

var (
	// group 1: key + separator + opening quote; group 2: value.
	quotedAssign = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*` + secretKeyWords + `\\?["']?\s*[:=]\s*\\?["'])([^"'\\\s][^"'\\\n]{3,199})`)
	// unquoted: value stops at whitespace / quote / delimiter.
	unquotedAssign = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*` + secretKeyWords + `\\?["']?\s*[:=]\s*)([A-Za-z0-9_+/=.~!@#$%^*-]{6,200})`)
)

// keepReplacer masks only capture group `group` (default: everything
// after group 1), keeping the key/prefix so context survives. accept, if
// set, vetoes matches whose value does not look like a credential.
type keepReplacer struct {
	re     *regexp.Regexp
	group  int
	accept func(string) bool
	// hints: lowercase substrings, at least one of which must occur in
	// the input for the pattern to run (performance prefilter only).
	hints []string
}

func (k *keepReplacer) ReplaceAllString(s, repl string) string {
	locs := k.re.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		// Value: capture group k.group, or (group 0) everything after
		// group 1 up to the end of the match.
		start, end := m[3], m[1]
		if k.group > 0 {
			start, end = m[2*k.group], m[2*k.group+1]
		}
		if start < 0 || end <= start {
			continue
		}
		if k.accept != nil && !k.accept(s[start:end]) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(repl)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func credentialLike(v string) bool { return mixedClasses(2, 0)(v) }

// mixedClasses accepts values of at least minLen that mix at least n of
// {lower, upper, digit, symbol}.
func mixedClasses(n, minLen int) func(string) bool {
	return func(v string) bool {
		if len(v) < minLen {
			return false
		}
		var lo, up, di, sy bool
		for _, r := range v {
			switch {
			case unicode.IsLower(r):
				lo = true
			case unicode.IsUpper(r):
				up = true
			case unicode.IsDigit(r):
				di = true
			default:
				sy = true
			}
		}
		c := 0
		for _, f := range []bool{lo, up, di, sy} {
			if f {
				c++
			}
		}
		return c >= n
	}
}

// luhnSafeCC is a custom replacer rather than a plain regexp because
// 13-19-digit sequences are common in real prose (port numbers,
// transaction IDs, build hashes). We only mask numbers that pass the
// Luhn check, dramatically cutting false positives.
var luhnSafeCC = &luhnReplacer{
	re: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
}

type luhnReplacer struct {
	re *regexp.Regexp
}

// ReplaceAllString satisfies the same surface as *regexp.Regexp so
// applyAll can treat it uniformly.
func (l *luhnReplacer) ReplaceAllString(s, repl string) string {
	return l.re.ReplaceAllStringFunc(s, func(match string) string {
		digits := stripNonDigits(match)
		if len(digits) < 13 || len(digits) > 19 {
			return match
		}
		if !luhn(digits) {
			return match
		}
		return repl
	})
}

func stripNonDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhn returns true if the digit string passes the Luhn checksum.
func luhn(digits string) bool {
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
