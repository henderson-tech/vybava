package secretscan

import (
	"strings"
	"testing"
)

// Fixture secrets are assembled at run time so neither the commit guard nor
// a host's push protection ever sees one in the source.
func fake(prefix string, n int) string {
	const alphabet = "a7Kq2Xm9Pz4Rt8Vb3Nc6Hw1Ly5Jd0Fs"
	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; b.Len() < len(prefix)+n; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

// planted splits text marked ⟦like this⟧ into the text and the spans the
// markers enclose.
func planted(marked string) (string, [][2]int) {
	var b strings.Builder
	var spans [][2]int
	start := -1
	for _, r := range marked {
		switch r {
		case '⟦':
			start = b.Len()
		case '⟧':
			spans = append(spans, [2]int{start, b.Len()})
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), spans
}

func TestFindRedactsEachClassAndOnlyTheValue(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + fake("MIIE", 64) + "\n" + fake("", 40) + "\n-----END RSA PRIVATE KEY-----"
	cases := []struct {
		name      string
		marked    string
		detectors string
	}{
		{"github token", "token: ⟦" + fake("gh"+"p_", 36) + "⟧ ok", "github-token"},
		{"aws key", "id=⟦" + "AKIA" + strings.ToUpper(fake("", 16)) + "⟧", "aws-key"},
		{"anthropic key in JSON", `{"key":"⟦` + fake("sk-"+"ant-api03-", 40) + `⟧"}`, "anthropic-key"},
		{"stripe key", "⟦" + fake("sk_"+"live_", 24) + "⟧", "stripe-key"},
		{"jwt whole", "⟦" + fake("ey"+"J", 24) + "." + fake("ey"+"J", 30) + "." + fake("", 20) + "⟧", "jwt"},
		{"private key block", "cat id\n⟦" + pem + "⟧\ndone", "private-key"},
		{"private key with literal \\n", `⟦-----BEGIN OPENSSH PRIVATE KEY-----\n` + fake("b3Bl", 70) + `\n-----END OPENSSH PRIVATE KEY-----⟧`, "private-key"},
		{"url credential", "DATABASE_URL=postgres://app:⟦" + fake("", 20) + "⟧@db.internal:5432/app", "url-credential"},
		{"url credential no user", "redis://:⟦" + fake("", 16) + "⟧@cache:6379", "url-credential"},
		{"bearer header", "-H 'Authorization: Bearer ⟦" + fake("", 32) + "⟧'", "auth-header"},
		{"credential quoted with spaces", `password: "⟦correct horse 42 staple⟧"`, "credential-assignment"},
		{"credential unquoted", "password=⟦hunter22x⟧&next=1", "credential-assignment"},
		{"credential escaped JSON", `{\"api_key\":\"⟦` + fake("", 24) + `⟧\"}`, "credential-assignment"},
		{"cli flag", "mysql --password=⟦" + fake("", 12) + "⟧ -h db", "cli-flag"},
		{"env assignment", "MAIL_PASSWORD=⟦" + fake("", 20) + "⟧\nMAIL_HOST=smtp.example.org", "env-assignment"},
		{"env assignment json", `"TWILIO_TOKEN": "⟦` + fake("", 32) + `⟧"`, "env-assignment"},
		{"env yaml", "  APP_KEY: ⟦base64:" + fake("", 32) + "⟧", "env-assignment"},
		{"multi-occurrence line", "A_TOKEN=⟦" + fake("", 20) + "⟧ B_SECRET=⟦" + fake("", 20) + "⟧", "env-assignment env-assignment"},
		{"incident fragment", "MAIL_PASSWORD=<set len 44 prefix ⟦" + fake("", 7) + "⟧…>", "fragment"},
		{"fragment len= head=", "len=44 head=⟦" + fake("", 6) + "⟧", "fragment"},
		{"fragment ellipsis", "prefix ⟦" + fake("", 8) + "⟧...", "fragment"},
		{"token cut short", "gh" + "p_⟦abcd12⟧… is set", "fragment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, want := planted(c.marked)
			got := Find(text, All, nil)
			var ids []string
			for i, s := range got {
				ids = append(ids, s.Detector)
				if i < len(want) && (s.Start != want[i][0] || s.End != want[i][1]) {
					t.Errorf("span %d = %q, want %q", i, text[s.Start:s.End], text[want[i][0]:want[i][1]])
				}
			}
			if strings.Join(ids, " ") != c.detectors || len(got) != len(want) {
				t.Errorf("detectors = %v, want %s (spans %d, want %d)", ids, c.detectors, len(got), len(want))
			}
		})
	}
}

func TestFindLeavesNamesCodeAndPlaceholdersAlone(t *testing.T) {
	for _, text := range []string{
		"grep -rlE 'MAIL_PASSWORD|TWILIO_TOKEN' .",
		"MAIL_PASSWORD=${MAIL_PASSWORD}",
		"echo ${MAIL_PASSWORD:-fallback123}",
		"GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
		"API_KEY=onyx://Vybava/Stripe/key",
		"STRIPE_PUBLISHABLE_KEY=pk_live_abc123def456",
		"SORT_KEY=createdAt",
		"PWD=/Users/someone/Work/app",
		"SECRET_NAME=MAIL_PASSWORD",
		`const EnvPassword = "POSTA_APP_PASSWORD"`,
		"password: hashedPassword",
		"password: z.string().min(8)",
		"secret: process.env.SECRET",
		"access_token: response.data.access_token",
		"MAIL_PASSWORD=<set len 44 [REDACTED]>",
		"MAIL_PASSWORD=[REDACTED:env-assignment]*****",
		"TWILIO_TOKEN=********************",
		`echo "$v=<set len ${#x} prefix $(echo $x | cut -c1-8)>"`,
		"-----BEGIN RSA PRIVATE KEY----- is the header to look for",
		"Bearer token authentication is configured",
		"docker login --password-stdin",
		"Set TWILIO_TOKEN: required",
	} {
		if got := Find(text, All, nil); len(got) != 0 {
			t.Errorf("Find(%q) = %v, want none", text, got)
		}
	}
}

func TestFillIsSameLengthAndASecondPassFindsNothing(t *testing.T) {
	text := "MAIL_PASSWORD=" + fake("", 44) + " token " + fake("gh"+"p_", 36) + " len 44 prefix " + fake("", 7) + "…"
	spans := Find(text, All, nil)
	if len(spans) != 3 {
		t.Fatalf("spans = %v, want 3", spans)
	}
	b := []byte(text)
	for _, s := range spans {
		copy(b[s.Start:s.End], Fill(s.Detector, s.End-s.Start))
	}
	if len(b) != len(text) {
		t.Fatalf("length changed %d → %d", len(text), len(b))
	}
	if again := Find(string(b), All, nil); len(again) != 0 {
		t.Errorf("second pass found %v in %q", again, b)
	}
}

func TestKnownValuesAndDotenv(t *testing.T) {
	secret := fake("", 24)
	var k Known
	if n, err := k.AddDotenv([]byte("APP_URL=https://app.example.org\n# MAIL_PASSWORD=old\nexport MAIL_PASSWORD=\"" + secret + "\"\nSHORT_TOKEN=abc\n")); n != 1 || err != nil {
		t.Fatalf("AddDotenv took %d, want 1", n)
	}
	text := "log: connecting with " + secret + " to https://app.example.org"
	got := Find(text, All, &k)
	if len(got) != 1 || got[0].Detector != "known" || text[got[0].Start:got[0].End] != secret {
		t.Errorf("Find with known = %v", got)
	}
}

func TestShapeNeverCarriesTheValue(t *testing.T) {
	secret := fake("", 30)
	text := "for v in A; do echo MAIL_PASSWORD=" + secret + " near " + fake("x", 20) + "\n"
	spans := Find(text, All, nil)
	if len(spans) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	shape := Shape(text, spans[0])
	if strings.Contains(shape, secret[:6]) || !strings.Contains(shape, "MAIL_PASSWORD=‹env-assignment›") || !strings.Contains(shape, "<w>") {
		t.Errorf("Shape = %q", shape)
	}
}

// Quote serves messages about a line some rule already flagged: nothing
// after the first secret or assignment may reach them, recognised or not.
func TestQuoteWithholdsEverythingAfterTheFirstSecret(t *testing.T) {
	unrecognised := "abcdefghijk"
	for _, line := range []string{
		"+creds " + "AKIA" + strings.ToUpper(fake("", 16)) + " otherCredential=" + unrecognised,
		"+otherCredential=" + unrecognised,
	} {
		q := Quote(line)
		if strings.Contains(q, unrecognised) || strings.Contains(q, "AKIA") && strings.Contains(q, strings.ToUpper(fake("", 16))) {
			t.Errorf("Quote(%q) = %q carries a value", line, q)
		}
	}
	if q := Quote("+token " + fake("gh"+"p_", 36)); !strings.Contains(q, "github-token") {
		t.Errorf("Quote must name the detector: %q", q)
	}
}

func TestShapeMasksEveryNeighbourButVariableNames(t *testing.T) {
	var k Known
	k.Add("knownpart")
	text := "MAIL_PASSWORD=knownpartsecretrest other words"
	spans := Find(text, All, &k)
	if len(spans) == 0 {
		t.Fatal("no span")
	}
	shape := Shape(text, spans[0])
	for _, leak := range []string{"secretrest", "knownpart", "other", "words"} {
		if strings.Contains(shape, leak) {
			t.Errorf("Shape = %q shows %q", shape, leak)
		}
	}
	if !strings.Contains(shape, "MAIL_PASSWORD=") {
		t.Errorf("Shape = %q lost the variable name", shape)
	}
	// SCREAMING_SNAKE in value position is a value, not a name.
	text = "MAIL_PASSWORD=knownpart TOP_SECRET"
	if shape = Shape(text, Find(text, All, &k)[0]); strings.Contains(shape, "TOP_SECRET") {
		t.Errorf("Shape = %q shows an uppercase value", shape)
	}
	text = `echo "TOP_SECRET" = knownpart`
	if shape = Shape(text, Find(text, All, &k)[0]); strings.Contains(shape, "TOP_SECRET") {
		t.Errorf("Shape = %q shows a quoted uppercase argument", shape)
	}
	text = `{"MAIL_PASSWORD": "knownpart"}`
	if shape = Shape(text, Find(text, All, &k)[0]); !strings.Contains(shape, `"MAIL_PASSWORD":`) {
		t.Errorf("Shape = %q lost a JSON key", shape)
	}
}

func TestAddDotenvReportsALineItCannotRead(t *testing.T) {
	var k Known
	long := "HUGE_TOKEN=" + strings.Repeat("x", 2<<20) + "\nMAIL_PASSWORD=" + fake("", 20) + "\n"
	if _, err := k.AddDotenv([]byte(long)); err == nil {
		t.Error("an over-long line silently ended the load")
	}
}
