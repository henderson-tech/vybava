package gitkit

import (
	"fmt"
	"slices"
	"strings"
)

// verbArgs declares everything one native verb accepts; parse refuses the
// rest. A verb that silently drops an argument does the wrong thing quietly:
// on 2026-09-25 `github-io create-pr` ignored `--repo <path>` (the PR went to
// the cwd's repository) and `--body-file` (the PR fell back to the commit
// log), and `merge-precheck --repo <path> 104` read the path as the PR.
type verbArgs struct {
	values      []string // flags taking a value: --key value or --key=value
	bools       []string // flags taking none
	positionals int      // most positional arguments; -1 = any number
	usage       string   // shown with every refusal
}

// parse validates argv against the declaration. It returns each flag's value
// (a boolean flag maps to "") and the positionals in order, so a verb never
// scans argv itself and never mistakes a flag's value for a positional.
func (s verbArgs) parse(verb string, argv []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var pos []string
	refuse := func(format string, a ...any) (map[string]string, []string, error) {
		return nil, nil, fmt.Errorf("%s: %s\n%s", verb, fmt.Sprintf(format, a...), s.usage)
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, isFlag := strings.CutPrefix(arg, "--")
		if !isFlag {
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return refuse("unknown argument %s (flags are spelled --name)", arg)
			}
			if s.positionals >= 0 && len(pos) == s.positionals {
				return refuse("unexpected argument %q", arg)
			}
			pos = append(pos, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(name, "=")
		switch {
		case slices.Contains(s.values, name):
			if hasInline {
				flags[name] = inline
				continue
			}
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return refuse("--%s needs a value", name)
			}
			i++
			flags[name] = argv[i]
		case slices.Contains(s.bools, name):
			if hasInline {
				return refuse("--%s takes no value", name)
			}
			flags[name] = ""
		default:
			return refuse("unknown argument --%s", name)
		}
	}
	return flags, pos, nil
}
