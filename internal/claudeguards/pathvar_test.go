package claudeguards

import "testing"

// The loop and assignment shapes from the 2026-09-24→27 transcripts are
// refused; other names, bash payloads and quoted text are not.
func TestPathVariable(t *testing.T) {
	for cmd, want := range map[string]bool{
		`for path in apps/api apps/web; do wc -l "$path"/package.json; done`: true,
		`git ls-files '*.ts' | while read path; do head -1 "$path"; done`:    true,
		`git ls-files | while IFS= read -r path; do echo "$path"; done`:      true,
		`path=$(git rev-parse --show-toplevel); ls "$path"`:                  true,
		`cd /tmp && path=/tmp/x.log; tail -2 "$path"`:                        true,
		`FOO=1 path=/tmp/x ls`:                                         true,
		`FOO=1 path+=(/tmp/x) ls`:                                      true,
		`FOO+=(one two) path=/tmp ls`:                                  true,
		`FOO='a b' BAR="c d" path=/tmp ls`:                             true,
		`FOO+=(one two) BAR=x ls`:                                      false,
		`local path=/tmp/x; ls`:                                        true,
		`export path=/tmp/x; ls`:                                       true,
		`(for path in a b; do echo $path; done)`:                       true,
		`for p in apps/api apps/web; do wc -l "$p"/package.json; done`: false,
		`git ls-files | while read -r file; do head -1 "$file"; done`:  false,
		`PATH=/usr/bin:$PATH ls`:                                       false,
		`echo "for path in a b"`:                                       false,
		`grep -n 'path=' config.ts`:                                    false,
		`bash -c 'for path in a b; do echo $path; done'`:               false,
		`cat > /tmp/x.sh <<'EOF'
for path in a b; do echo $path; done
EOF
bash /tmp/x.sh`: false,
		`ls -la ~/.claude/skills/my/path/`:                                        false,
		`git diff origin/main -- packages/path/src`:                               false,
		`node -e 'const path = require("path"); console.log(path.join("a","b"))'`: false,
	} {
		if _, got := pathVariableUse(cmd); got != want {
			t.Errorf("%s: denied=%v, want %v", cmd, got, want)
		}
	}
	if d := guardPathVariable(bashInput("", `for path in a b; do ls $path; done`)); d == nil || d.Rule != "shell:path-variable" {
		t.Fatalf("guardPathVariable: got %+v", d)
	}
}
