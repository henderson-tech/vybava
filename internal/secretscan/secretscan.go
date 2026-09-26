// Package secretscan is the one catalogue of what a leaked secret looks like in
// text: provider token shapes, credential and secret-named env assignments,
// URL credentials, auth headers and the "length + prefix" fragments an agent
// prints when it believes a fragment is harmless. claude-guards' commit scan,
// memorylint and the transcript redactor all read it — a new shape is one row
// here, never a local regex.
//
// It reports spans (byte offsets + detector id), never copies of values, and
// callers must keep it that way: a finding is rendered with Mask or Shape, and
// a matched value never reaches output, a log or an error.
package secretscan

import (
	"regexp"
	"sort"
	"strings"
)

// Class groups detectors by how strong their evidence is.
type Class uint8

const (
	// Tokens are provider shapes that are secrets wherever they appear.
	Tokens Class = 1 << iota
	// Credentials are values bound to a credential word: password/secret
	// assignments, URL userinfo, auth headers, --password style flags.
	Credentials
	// EnvValues are values of SCREAMING_SNAKE names that carry secrets
	// (MAIL_PASSWORD=…, TWILIO_TOKEN: …).
	EnvValues
	// Fragments are partial disclosures: `len 44 prefix abcdefg…`, `ghp_abcd…`.
	Fragments
	// All is every class.
	All = Tokens | Credentials | EnvValues | Fragments
)

// Span is one finding: text[Start:End] is secret material.
type Span struct {
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Detector string `json:"detector"`
}

type detector struct {
	id    string
	class Class
	re    *regexp.Regexp
	// group selects the submatch that is the secret; 0 is the whole match.
	group int
	// accept vets and adjusts one match (text, submatch indexes) and returns
	// the span to redact; ok=false drops it. nil keeps [group] as-is.
	accept func(text string, m []int) (start, end int, ok bool)
	// hints are literals every match contains one of (lower-case against
	// lower-cased text with fold): the regex only runs when one is present.
	hints []string
	fold  bool
	// window runs the regex only within windowLead/windowTail bytes of each
	// hint, never over the whole text: a pattern with no literal prefix
	// (\b[A-Z]…, (?i)…) otherwise walks Go's NFA over every byte of a
	// multi-MB tool output. Only for matches that sit near their hint and
	// are bounded in length — or whose accept extends them over the text.
	window bool
}

const windowLead, windowTail = 128, 512

var credentialHints = []string{"password", "passwd", "secret", "api_key", "api-key", "apikey",
	"access_token", "access-token", "accesstoken", "auth_token", "auth-token", "authtoken"}

// matches returns d's submatch indexes over text, or nil when no hint is in it.
func (d detector) matches(text string, lower func() string) [][]int {
	in := text
	if d.fold {
		in = lower()
	}
	if !d.window {
		for _, h := range d.hints {
			if strings.Contains(in, h) {
				return d.re.FindAllStringSubmatchIndex(text, -1)
			}
		}
		return nil
	}
	var at []int
	for _, h := range d.hints {
		for from := 0; ; {
			i := strings.Index(in[from:], h)
			if i < 0 {
				break
			}
			at = append(at, from+i)
			from += i + len(h)
		}
	}
	sort.Ints(at)
	var out [][]int
	ws, we := -1, -1
	flush := func() {
		if ws < 0 {
			return
		}
		for _, m := range d.re.FindAllStringSubmatchIndex(text[ws:we], -1) {
			for i := range m {
				if m[i] >= 0 {
					m[i] += ws
				}
			}
			out = append(out, m)
		}
	}
	for _, p := range at {
		s, e := max(0, p-windowLead), min(len(text), p+windowTail)
		if ws >= 0 && s <= we {
			we = max(we, e)
			continue
		}
		flush()
		ws, we = s, e
	}
	flush()
	return out
}

// asciiLower lower-cases ASCII letters only, so every offset into it is an
// offset into the original — strings.ToLower may change a byte length.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// Token shapes. tokenPattern (the alternation of their re) is the commit
// guard's line check, so a row here widens what a commit refuses.
var tokenDetectors = []detector{
	{id: "private-key", class: Tokens, hints: []string{"PRIVATE KEY"}, re: regexp.MustCompile(`BEGIN [A-Z ]*PRIVATE KEY`), accept: privateKeyBlock},
	{id: "aws-key", class: Tokens, hints: []string{"AKIA"}, re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{id: "github-token", class: Tokens, hints: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{id: "slack-token", class: Tokens, hints: []string{"xox"}, re: regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{id: "anthropic-key", class: Tokens, hints: []string{"sk-ant-"}, re: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{id: "openai-key", class: Tokens, hints: []string{"sk-"}, re: regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{40,}`)},
	{id: "stripe-key", class: Tokens, window: true, hints: []string{"k_live_", "k_test_"}, re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{id: "google-api-key", class: Tokens, hints: []string{"AIza"}, re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{id: "npm-token", class: Tokens, window: true, hints: []string{"npm_"}, re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{id: "jwt", class: Tokens, hints: []string{"eyJ"}, re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.eyJ[A-Za-z0-9_-]*(?:\.[A-Za-z0-9_-]*)?`)},
}

var tokenPattern = func() *regexp.Regexp {
	parts := make([]string, len(tokenDetectors))
	for i, d := range tokenDetectors {
		parts[i] = d.re.String()
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}()

// ContainsToken reports whether s carries any token shape. It is a line
// check: a private-key header alone counts, which Find (wanting the key
// body to redact) does not.
func ContainsToken(s string) bool { return tokenPattern.MatchString(s) }

const (
	// credentialWord is the vocabulary that makes an assignment a credential.
	credentialWord = `(?:password|passwd|secret|api[_-]?key|access[_-]?token|client[_-]?secret|auth[_-]?token)`
	// unquotedValue stops where shell, JSON and prose delimit a value.
	unquotedValue = "[^\\s\"'`,;&|)}\\]<>]"
)

var (
	// The commit guard's rule for one added diff line: a quoted value of 8+.
	rePassword     = regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|access[_-]?token)["']?[[:space:]]*[:=][[:space:]]*["'][^"']{8,}`)
	rePasswordSkip = regexp.MustCompile(`\$\{|\$\(|process\.env|os\.Getenv|os\.environ|System\.getenv|Deno\.env\.get|secrets\.|vars\.|example|placeholder|changeme|<[^>]+>`)

	// An assignment whose IDENTIFIER names an environment variable ("…env…")
	// and whose VALUE is a bare SCREAMING_SNAKE identifier. That value is the
	// KEY handed to os.Getenv / process.env[…], never a credential:
	//   const EnvPassword = "POSTA_APP_PASSWORD"   → not a secret
	//   var   password    = "ADMIN_PASSWORD"       → STILL a secret
	// BOTH signals are required, because either one alone blinds the rule to a
	// real leak. At least one underscore is required too, so a caps-only token
	// with real entropy (base32 TOTP seed, uppercase hex key) keeps counting as
	// a secret.
	// The (?i) is scoped to the identifier on purpose: letting it reach the
	// value would make the SCREAMING_SNAKE alternation case-insensitive, and
	// `envKey = "sk_live_9f8a…"` would suppress a real leak.
	reEnvNameConst = regexp.MustCompile(`\b(?i:[a-z0-9_]*env[a-z0-9_]*)[[:space:]]*:?=[[:space:]]*` +
		`(?:"[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+"|'[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+')`)
)

// CredentialAssignment reports whether one added diff line hardcodes a
// credential — the commit guard's rule, where the three signals (shape,
// placeholder, env-var name) are weighed together over the whole line.
func CredentialAssignment(l string) bool {
	return rePassword.MatchString(l) &&
		!rePasswordSkip.MatchString(l) &&
		!reEnvNameConst.MatchString(l)
}

var (
	// secretNameTail is the last word of an env name that carries a secret.
	secretNameTail = regexp.MustCompile(`(?:^|_)(?:PASSWORD|PASSWD|PASS|PWD|SECRET|TOKEN|KEY|KEY_BASE|APIKEY|DSN|PASSPHRASE|CREDENTIALS?|COOKIE|PAT)(?:_[0-9]+|_B64|_BASE64)?$`)
	publicName     = regexp.MustCompile(`(?:PUBLIC|PUBLISHABLE|SORT|CACHE|PRIMARY|FOREIGN|PARTITION|IDEMPOTENCY|ROUTING|LOOKUP|OBJECT)_KEY$`)
	// strongName marks names whose value is a secret whatever it looks like;
	// a bare *_KEY must also look random (SORT_KEY=createdAt is not).
	strongName  = regexp.MustCompile(`(?:PASSWORD|PASSWD|PASS|PWD|SECRET|TOKEN|APIKEY|API_KEY|ACCESS_KEY|PRIVATE_KEY|SECRET_KEY|APP_KEY|AUTH_KEY|MASTER_KEY|ENCRYPTION_KEY|SIGNING_KEY|LICENSE_KEY|DSN|PASSPHRASE|CREDENTIALS?|COOKIE)(?:_[0-9]+|_B64|_BASE64)?$`)
	reScreaming = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// SecretName reports whether an environment variable name carries a secret
// value: MAIL_PASSWORD, TWILIO_TOKEN, APP_KEY, SENTRY_DSN — never PUBLIC_KEY.
func SecretName(name string) bool {
	return !notSecretAlone[name] && reScreaming.MatchString(name) && secretNameTail.MatchString(name) && !publicName.MatchString(name)
}

// notSecretAlone are tails that name something else when they stand alone:
// the shell's working directory, a grep pattern, a test counter (PASS=0).
var notSecretAlone = map[string]bool{"PWD": true, "PAT": true, "PASS": true}

// Catalogue order names a span two detectors agree on, so the specific ones
// (a URL's userinfo, a flag, a secret-named env var) precede the generic
// credential assignment.
var otherDetectors = []detector{
	// scheme://user:password@host — DATABASE_URL, REDIS_URL, git remotes.
	{id: "url-credential", class: Credentials, group: 1, window: true, hints: []string{"://"},
		re:     regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]{1,20}://[^\s/:@"'<>]{0,128}:([^\s/@"'<>]{3,256})@[A-Za-z0-9.\[\]_-]`),
		accept: plausibleValue(3)},
	// Authorization: Bearer … and a bare `Bearer <token>`.
	{id: "auth-header", class: Credentials, group: 1, window: true, fold: true, hints: []string{"bearer", "basic", "token", "bot"},
		re:     regexp.MustCompile(`(?i)\b(?:bearer|basic|token|bot)[ \t]+([A-Za-z0-9._~+/-]{16,}=*)`),
		accept: authHeader},
	// --password=xyz · --token xyz · -p'xyz' (mysql) is too ambiguous to take.
	{id: "cli-flag", class: Credentials, group: 1, window: true, fold: true, hints: []string{"-pass", "-token", "-api", "-secret", "-client", "-auth"},
		re:     regexp.MustCompile(`(?i)(?:^|\s)--?(?:password|passwd|pass|token|api-key|apikey|secret|client-secret|auth-token)[= ]["']?(` + unquotedValue + `{6,256})`),
		accept: plausibleValue(6)},
	// NAME=value · NAME: value · "NAME": "value" · export NAME='value'.
	{id: "env-assignment", class: EnvValues, window: true, hints: []string{"PASS", "PWD", "SECRET", "TOKEN", "KEY", "DSN", "CREDENTIAL", "COOKIE", "PAT"},
		re:     regexp.MustCompile(`\b([A-Z][A-Z0-9_]{1,80})\\?["']?[ \t]*[:=][ \t]*(\\?["'])?`),
		accept: envValue},
	// password: hunter22 · "api_key": "…" · secret='…' — the value is read by
	// valueAt, so a quoted one runs to its closing quote, spaces included.
	{id: "credential-assignment", class: Credentials, window: true, fold: true, hints: credentialHints,
		re:     regexp.MustCompile(`(?i)` + credentialWord + `\\?["']?[ \t]*[:=][ \t]*(\\?["'])?`),
		accept: credentialValue},
	// `len 44 prefix abcdefg…` · `length=44, first 8 chars: abcdefgh`.
	{id: "fragment", class: Fragments, group: 1, window: true, fold: true, hints: []string{"len"},
		re:     regexp.MustCompile(`(?i)\blen(?:gth)?[ \t]*[=:]?[ \t]*\d+[ \t]*[,;]?[ \t]*(?:prefix|head|starts?(?:[ \t]+with)?|begins?(?:[ \t]+with)?|first(?:[ \t]*\d+)?(?:[ \t]*chars?)?)[ \t]*[=:]?[ \t]*["'` + "`" + `]?([^\s"'` + "`" + `>…,)]{2,64})`),
		accept: plausibleValue(2)},
	// `prefix=abcdefg…` — a truncation marker right after the value.
	{id: "fragment", class: Fragments, group: 1, window: true, hints: []string{"…", "..."},
		re:     regexp.MustCompile(`(?i)\b(?:prefix|head|starts[ \t]+with|first[ \t]*\d+[ \t]*chars?)[ \t]*[=:]?[ \t]*["'` + "`" + `]?([A-Za-z0-9+/_=-]{3,64})(?:…|\.\.\.)`),
		accept: plausibleValue(3)},
	// A token shape cut short for display: `ghp_abcd…`, `sk-ant-api03-xy...`.
	{id: "fragment", class: Fragments, group: 1, window: true, hints: []string{"…", "..."},
		re:     regexp.MustCompile(`(?:gh[pousr]_|github_pat_|sk-ant-|sk-proj-|xox[baprs]-|AKIA|AIza|[sr]k_(?:live|test)_|npm_)([A-Za-z0-9_-]{3,40})(?:…|\.\.\.)`),
		accept: plausibleValue(3)},
}

// minSecretText is shorter than any text a detector or a Known value (MinKnown)
// can match: `len 4 prefix ab` is the shortest.
const minSecretText = 8

// Find returns the spans of secret material in text, sorted, overlaps merged
// (the earlier detector in catalogue order names a merged span). known adds
// exact values supplied out of band — a vault item, a .env — as "known".
func Find(text string, classes Class, known *Known) []Span {
	if len(text) < minSecretText {
		return nil
	}
	var lowered string
	lower := func() string {
		if lowered == "" {
			lowered = asciiLower(text)
		}
		return lowered
	}
	var spans []Span
	for _, list := range [][]detector{tokenDetectors, otherDetectors} {
		for _, d := range list {
			if classes&d.class == 0 {
				continue
			}
			for _, m := range d.matches(text, lower) {
				start, end, ok := m[2*d.group], m[2*d.group+1], true
				if d.accept != nil {
					start, end, ok = d.accept(text, m)
				}
				if ok && start >= 0 && end > start {
					spans = append(spans, Span{Start: start, End: end, Detector: d.id})
				}
			}
		}
	}
	spans = append(spans, known.find(text)...)
	return merge(spans)
}

func merge(spans []Span) []Span {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	out := spans[:1]
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s.Start < last.End {
			last.End = max(last.End, s.End)
			continue
		}
		out = append(out, s)
	}
	return out
}

// privateKeyBlock widens a PRIVATE KEY header to the whole block, dashes and
// END line included — and drops a header with no key body after it, which is
// code or prose about keys, not a key.
func privateKeyBlock(text string, m []int) (int, int, bool) {
	start := m[0]
	for start > 0 && text[start-1] == '-' {
		start--
	}
	rest := text[m[1]:min(len(text), m[1]+16384)]
	body := len(rest)
	if i := strings.Index(rest, "-----END"); i >= 0 {
		body = i
	}
	payload := 0
	for i := 0; i < body; i++ {
		c := rest[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=':
			payload++
		case c == '-', c == ' ', c == '\t', c == '\r', c == '\n', c == '\\', c == ':', c == ',', c == '.':
		default:
			body = i // prose after a truncated key
		}
	}
	if payload < 40 {
		return 0, 0, false
	}
	end := m[1] + body
	if strings.HasPrefix(text[end:], "-----END") {
		if close := strings.Index(text[end+8:], "-----"); close >= 0 && close < 64 {
			end += 8 + close + 5
		}
	}
	return start, end, true
}

var placeholderExact = map[string]bool{
	"changeme": true, "placeholder": true, "redacted": true, "none": true, "null": true, "nil": true,
	"undefined": true, "empty": true, "unset": true, "required": true, "missing": true, "set": true,
	"true": true, "false": true, "secret": true, "password": true, "token": true, "string": true,
	"optional": true, "present": true, "hidden": true, "masked": true, "value": true,
}

var (
	rePlaceholder = regexp.MustCompile(`(?i)example|placeholder|changeme|dummy|your[_-]|xxxx|\*\*\*|•••|\[redacted|<redacted|redacted>|\.\.\.$|…`)
	// A value that is itself a reference, not a secret: an expansion, a
	// template, a vault handle, a code path.
	reReference = regexp.MustCompile(`^(?:\\?\$|%|\{\{|<|\[|@|onyx://|op://|vault:|secrets\.|vars\.|env\.|process\.env|os\.|getenv|System\.|Deno\.)`)
	reCodePath  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$|\(`)
	reMaskOnly  = regexp.MustCompile(`^[*•#xX.\-_]+$`)
)

// placeholder reports values that stand in for a secret rather than being one.
func placeholder(v string) bool {
	v = strings.Trim(v, `"'`+"`\\")
	return v == "" || placeholderExact[strings.ToLower(v)] || rePlaceholder.MatchString(v) ||
		reReference.MatchString(v) || reMaskOnly.MatchString(v)
}

// randomish reports a value carrying both letters and digits — the cheap
// randomness test that separates `hunter22` from `hashedPassword`.
func randomish(v string) bool {
	var letter, digit bool
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	return letter && digit
}

func plausibleValue(minLen int) func(string, []int) (int, int, bool) {
	return func(text string, m []int) (int, int, bool) {
		v := text[m[2]:m[3]]
		return m[2], m[3], len(v) >= minLen && !placeholder(v)
	}
}

func authHeader(text string, m []int) (int, int, bool) {
	v := text[m[2]:m[3]]
	// A bare "token <word>" in prose needs the header context to count.
	word := strings.ToLower(text[m[0] : m[0]+strings.IndexAny(text[m[0]:], " \t")])
	before := strings.ToLower(text[max(0, m[0]-24):m[0]])
	if (word == "token" || word == "bot") && !strings.Contains(before, "authorization") {
		return 0, 0, false
	}
	// The window may have cut a long token (a JWT bearer): finish it here.
	end := m[3]
	for end < len(text) && strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~+/=-", text[end]) >= 0 {
		end++
	}
	return m[2], end, !placeholder(v) && randomish(v)
}

// valueAt reads the value that starts at pos. After an opening quote (quote
// is its text, `"` or `\"` inside an encoded string) the value runs to the
// matching close on the same line; unquoted it runs to the first delimiter.
// ok=false for an unterminated quote: nothing proves where the value ends.
func valueAt(text string, pos int, quote string) (end int, ok bool) {
	limit := min(len(text), pos+512)
	if quote != "" {
		for i := pos; i < limit; i++ {
			if text[i] == '\n' {
				return 0, false
			}
			if strings.HasPrefix(text[i:], quote) {
				return i, true
			}
		}
		return 0, false
	}
	end = pos
	for end < limit && !strings.ContainsRune(" \t\r\n\"'`,;&|)}]<>\\", rune(text[end])) {
		end++
	}
	return end, true
}

func isIdent(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func credentialValue(text string, m []int) (int, int, bool) {
	var quote string
	if m[2] >= 0 {
		quote = text[m[2]:m[3]]
	}
	// The whole identifier the word ends: `EnvPassword`, `${DB_PASSWORD:-…}`.
	ident := m[0]
	for ident > 0 && isIdent(text[ident-1]) {
		ident--
	}
	if ident > 0 && (text[ident-1] == '$' || text[ident-1] == '{') {
		return 0, 0, false
	}
	start := m[1]
	end, ok := valueAt(text, start, quote)
	if !ok || end-start < 8 {
		return 0, 0, false
	}
	v := text[start:end]
	// An unquoted value must look random and not be code.
	if quote == "" && (reCodePath.MatchString(v) || !randomish(v)) {
		return 0, 0, false
	}
	region := text[ident:min(len(text), end+len(quote))]
	if placeholder(v) || reEnvNameConst.MatchString(region) || rePasswordSkip.MatchString(v) {
		return 0, 0, false
	}
	return start, end, true
}

func envValue(text string, m []int) (int, int, bool) {
	name := text[m[2]:m[3]]
	// ${NAME:-fallback} and $NAME are expansions, not assignments.
	if m[2] > 0 && (text[m[2]-1] == '$' || text[m[2]-1] == '{') || !SecretName(name) {
		return 0, 0, false
	}
	var quote string
	if m[4] >= 0 {
		quote = text[m[4]:m[5]]
	}
	start := m[1]
	end, ok := valueAt(text, start, quote)
	if !ok || end-start < 6 {
		return 0, 0, false
	}
	v := text[start:end]
	if placeholder(v) || rePasswordSkip.MatchString(v) || reCodePath.MatchString(v) {
		return 0, 0, false
	}
	// The value is another env name: `SECRET_NAME=MAIL_PASSWORD` names a key.
	if reScreaming.MatchString(v) && strings.Contains(v, "_") {
		return 0, 0, false
	}
	if !strongName.MatchString(name) && !randomish(v) {
		return 0, 0, false
	}
	return start, end, true
}
