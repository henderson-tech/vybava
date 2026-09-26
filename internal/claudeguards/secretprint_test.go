package claudeguards

import "testing"

func TestSecretPrintMatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string // rule name, "" = pass
	}{
		// --- the incident, verbatim in shape ---
		{"incident: ssh docker exec printenv captured, then len + prefix printed",
			`ssh produlinka 'docker exec reservine-sk-api sh -c "for v in MAIL_USERNAME MAIL_PASSWORD TWILIO_TOKEN; do x=\$(printenv \$v); if [ -n \"\$x\" ]; then echo \"\$v=<set len \${#x} prefix \$(echo \$x | cut -c1-8 | sed s/[A-Za-z0-9]\$//)>\"; else echo \"\$v=<EMPTY>\"; fi; done"' 2>&1`,
			"secret-fragment"},
		{"incident without printenv: indirect expansion + length",
			`for v in MAIL_PASSWORD TWILIO_TOKEN; do echo "$v len ${#v}"; x="${!v}"; echo "${x:0:7}"; done`,
			"secret-fragment"},

		// --- printenv ---
		{"printenv secret name", "printenv MAIL_PASSWORD", "printenv-secret"},
		{"printenv secret through docker", "docker exec app printenv STRIPE_SECRET", "printenv-secret"},
		{"printenv dynamic with secret list", `for v in APP_URL DB_PASSWORD; do printenv "$v"; done`, "printenv-secret"},
		{"printenv non-secret", "printenv APP_URL MAIL_HOST", ""},
		{"printenv dynamic, no secret named", `for v in APP_URL MAIL_HOST; do printenv "$v"; done`, ""},

		// --- fragments ---
		{"length of a secret expansion", "echo ${#MAIL_PASSWORD}", "secret-fragment"},
		{"substring of a secret expansion", `echo "${API_TOKEN:0:8}"`, "secret-fragment"},
		{"node slice of process.env", `node -e 'console.log(process.env.STRIPE_SECRET.slice(0, 7))'`, "secret-fragment"},
		{"python prefix of os.environ", `python3 -c 'import os; print(os.environ["APP_KEY"][:6])'`, "secret-fragment"},
		{"wc -c on a dotenv value is the .env class, out of scope", `grep ^APP_KEY= .env | cut -d= -f2 | wc -c`, ""},
		{"printenv length (field replay)", `printenv VITRINKA_TOKEN | wc -c`, "printenv-secret"},
		{"sha prefix of a git log is not a secret", "git log --format=%H | cut -c1-7", ""},
		{"length of PATH is not a secret", `node -e 'console.log(process.env.PATH.length)'`, ""},
		// Field replay: a secret and a fragment op in one command, not joined.
		{"token plus head -c of a response", `TOKEN=$(vitrinka auth token); printf 'Authorization: Bearer %s' "$TOKEN" | curl -s -o /tmp/r.json -H @- "$U"; echo "$(head -c 200 /tmp/r.json)"`, ""},
		{"sourced .env, curl, head -c", `set -a; . ~/secrets/.env; set +a; curl -s -u "admin:${ADMIN_PASSWORD}" "$B/x" | head -c 400`, ""},
		{"captured printenv used as a header", `ssh box 'k=$(docker exec litellm printenv LITELLM_MASTER_KEY); curl -s -H "Authorization: Bearer $k" http://127.0.0.1:4000/v1/models'`, ""},
		{"which are set, by name", `for v in CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY; do if [ -n "$(printenv $v)" ]; then echo "$v set"; fi; done`, ""},
		{"printenv to /dev/null", `printenv VITRINKA_TOKEN >/dev/null && echo has_token`, ""},

		// --- echo ---
		{"echo secret", `echo "$API_TOKEN"`, "secret-echo"},
		{"echo secret in text", `echo "token is ${GITHUB_TOKEN}"`, "secret-echo"},
		{"echo piped into its consumer", `echo "$GITHUB_TOKEN" | gh auth login --with-token`, ""},
		{"echo piped into cat still prints", `echo "$API_TOKEN" | cat`, "secret-echo"},
		{"echo piped into tee still prints", `echo "$API_TOKEN" | tee /tmp/out`, "secret-echo"},
		{"echo through tr into its consumer", `echo "$REGISTRY_TOKEN" | tr -d '\n' | docker login ghcr.io -u me --password-stdin`, ""},
		{"quoted pipe inside the printer's argument", `echo "$API_TOKEN" | cat 'a|b'`, "secret-echo"},
		{"quoted pipe inside a consumer's argument", `echo "$API_TOKEN" | curl -H @- --data 'x|y' "$U"`, ""},
		{"printer behind sudo", `echo "$API_TOKEN" | sudo tee /tmp/out`, "secret-echo"},
		{"printer behind xargs", `echo "$API_TOKEN" | xargs echo`, "secret-echo"},
		{"bare brace group prints", `{ echo "$API_TOKEN"; }`, "secret-echo"},
		{"function called bare prints", `H() { printf '%s' "$TOKEN"; }; H`, "secret-echo"},
		{"printf piped into docker login", `printf '%s' "$REGISTRY_PASSWORD" | docker login ghcr.io -u me --password-stdin`, ""},
		{"echo redirected to a file", `echo "$DEPLOY_KEY" > /tmp/key && chmod 600 /tmp/key`, ""},
		{"sanctioned is-it-set", `[ -n "$MAIL_PASSWORD" ] && echo set || echo MISSING`, ""},
		{"echo a name, not a value", `echo "MAIL_PASSWORD is missing"`, ""},
		{"single-quoted literal", `echo 'use $MAIL_PASSWORD here'`, ""},
		{"printf in a group piped to curl", `{ printf 'Authorization: Bearer %s\n' "$TOKEN"; printf 'X-Workspace: adf\n'; } | curl -s -H @- "$U"`, ""},
		{"printf in a function piped to curl", `H() { printf 'Authorization: Bearer %s' "$TOKEN"; }; H | curl -sS -H @- "$U"`, ""},
		{"PASS counter is not a password", `PASS=0; FAIL=0; echo "PASS=$PASS FAIL=$FAIL"`, ""},
		{"printf group inside a capture", `code=$({ printf 'Authorization: Bearer %s\n' "$TOKEN"; } | curl -s -o /dev/null -w '%{http_code}' -H @- "$U")`, ""},
		{"echo of a set-test", `ssh box "docker exec c sh -c 'echo TOKEN_SET=\$([ -n \"\$GATEWAY_TOKEN\" ] && echo yes || echo no)'"`, ""},
		{"echo of a :+ substitution", `echo "env set: ${VITRINKA_TOKEN:+yes}"`, ""},

		// --- .env printing is out of scope by decision (2026-09-26) ---
		{"cat .env passes", "cat .env", ""},
		{"commit message naming it", `git commit -m "never cat .env or printenv MAIL_PASSWORD"`, ""},

		// --- escape hatch ---
		{"escape hatch", "CLAUDE_ALLOW_DANGEROUS=1 printenv MAIL_PASSWORD", ""},
	}
	for _, c := range cases {
		if got := secretPrintMatch(c.cmd); got != c.want {
			t.Errorf("%s: secretPrintMatch(%q) = %q, want %q", c.name, c.cmd, got, c.want)
		}
	}
}
