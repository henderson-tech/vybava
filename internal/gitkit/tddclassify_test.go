package gitkit

import (
	"bytes"
	"testing"
)

func TestSkipCategory(t *testing.T) {
	cases := map[string][]string{
		// DB migrations
		"migration": {"apps/api/migrations/1718900000_add_col.ts", "prisma/migrations/20240101_init/migration.sql", "packages/db/src/migration/0007-foo.ts"},
		// lockfiles (package.json is not — a version bump there still gets judgment)
		"deps": {"pnpm-lock.yaml", "apps/web/package-lock.json", "bun.lock", "go.sum"},
		// CI/CD config wins over generic yaml
		"ci":        {".github/workflows/ci.yml", ".gitlab-ci.yml", ".circleci/config.yml"},
		"iac":       {"Dockerfile", "apps/api/Dockerfile.prod", "docker-compose.yml", "infra/main.tf", "nginx/site.conf", ".env.production"},
		"generated": {"src/api/types.generated.ts", "dist/index.js", "apps/web/src/orval.d.ts", "src/__generated__/schema.ts"},
		"docs":      {"README.md", "docs/guide.mdx", "CHANGELOG.txt"},
		// real source → apply the behavioral gates
		"": {"package.json", "apps/api/src/offer.service.ts", "src/utils/money.ts", "packages/core/lib/rateLimit.ts"},
	}
	for want, paths := range cases {
		for _, path := range paths {
			if got := SkipCategory(path); got != want {
				t.Errorf("SkipCategory(%q) = %q, want %q", path, got, want)
			}
		}
	}
}

func TestTDDClassifyVerb(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		code           int
		stdout, stderr string
	}{
		{[]string{"--json", "go.sum"}, 0, "deps\n", ""},
		{[]string{"src/a.ts"}, 0, "null\n", ""},
		{nil, 1, "", "error: usage: tdd-classify.ts <path>\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runTDDClassify(tc.args, &stdout, &stderr); code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
			t.Errorf("%v → %d %q %q; want %d %q %q", tc.args, code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
		}
	}
}
