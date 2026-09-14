package claudeguards

import "testing"

func TestEnvDumpMatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string // rule name, "" = pass
	}{
		// --- bare dumps ---
		{"env bare", "env", "env-dump"},
		{"printenv bare", "printenv", "env-dump"},
		{"env abs path", "/usr/bin/env", "env-dump"},
		{"env with assignment only", "env FOO=bar", "env-dump"},
		{"env piped to grep", "env | grep -i s3", "env-dump"},
		{"sudo env", "sudo env", "env-dump"},
		{"export bare", "export", "env-dump"},
		{"export -p", "export -p", "env-dump"},

		// --- the actual incident shape ---
		{"incident: ssh docker exec env grep sed",
			`ssh produlinka 'sudo docker exec vitrinka-green env 2>/dev/null | grep -i -E "blob|s3|bucket" | sed "s/SECRET.*/…/;s/KEY=.*/…/"'`,
			"env-dump"},
		{"docker exec env", "docker exec vitrinka-green env", "env-dump"},
		{"docker exec sh -c env", `docker exec c sh -c "env"`, "env-dump"},
		{"kubectl exec env", "kubectl exec pod-x -- env", "env-dump"},

		// --- runner form: env executes something, prints nothing ---
		{"env runner", "env FOO=bar ./run.sh", ""},
		{"env -i runner", "env -i bash -lc 'make build'", ""},
		{"env -u runner", "env -u DEBUG node app.js", ""},
		{"xargs env runner", "cat args | xargs env FOO=1 tool", ""},

		// --- specific names: deliberate allowlist reads ---
		{"printenv one name", "printenv PATH", ""},
		{"printenv several", "docker exec c printenv BLOB_BACKEND S3_ENDPOINT S3_BUCKET", ""},

		// --- name-only projection clears the pipeline ---
		{"cut names", "docker exec c env | cut -d= -f1", ""},
		{"cut names spaced", "env | cut -d = -f 1", ""},
		{"cut names reversed flags", "env | cut -f1 -d=", ""},
		{"awk names", "ssh h 'docker exec c env' | awk -F= '{print $1}'", ""},
		{"sed names", "env | sed 's/=.*//'", ""},
		{"grep is not a name filter", "env | grep -c S3 | cut -c1", "env-dump"},

		// --- field audit: an assignment prefix is not a disguise ---
		{"assignment prefix", "FOO=1 env", "env-dump"},
		{"valueless assignment prefix", "FOO= env", "env-dump"},
		{"quoted assignment prefix", `FOO='a b' printenv`, "env-dump"},

		// --- field audit: a quoted pattern is data, not a pipeline ---
		{"alternation branch named env", `grep -nE "vault|env|path" a.ts`, ""},
		{"alternation branch named printenv", `rg "a|printenv|b" f.ts`, ""},

		// --- /proc environ ---
		{"cat proc environ", "cat /proc/1234/environ", "proc-environ"},
		{"tr proc environ", `tr '\0' '\n' < /proc/self/environ`, "proc-environ"},
		{"ssh proc environ substituted", `ssh h 'strings /proc/$(pgrep app)/environ'`, "proc-environ"},

		// --- docker inspect Config.Env ---
		{"inspect config env", `docker inspect c --format '{{json .Config.Env}}'`, "inspect-config-env"},
		{"inspect range env", `docker inspect c --format '{{range .Config.Env}}{{.}}{{end}}'`, "inspect-config-env"},
		{"inspect status ok", `docker inspect c --format '{{.State.Status}}'`, ""},
		{"inspect mounts ok", `docker inspect vitrinka-green --format "{{range .Mounts}}{{.Source}}{{end}}"`, ""},

		// --- subcommands named env are not dumps ---
		{"go env", "go env GOPATH", ""},
		{"go env bare", "go env", ""},
		{"conda env list", "conda env list", ""},
		{"npm run build", "npm run build && env FOO=1 vite", ""},
		{"grep env in file", "grep -rn env internal/web/", ""},
		{"dotenv file ok", "cat .env.example", ""},
		{"export assignment ok", "export FOO=bar", ""},

		// --- prose + escape hatch ---
		{"echo env ok", "echo env is dumped here", ""},
		{"comment ok", "# never run env on prod", ""},
		{"escape hatch", "CLAUDE_ALLOW_DANGEROUS=1 env", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := envDumpMatch(c.cmd); got != c.want {
				t.Fatalf("cmd=%q: got %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}
