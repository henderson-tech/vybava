package lok

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Key grammar. A path-style key is its segments joined by an unescaped ".";
// inside a segment `\.` is a literal dot and `\\` a literal backslash. Any
// other backslash sequence, or a trailing `\`, is refused, which keeps room
// for future escapes. An english-as-key key IS the English text: it is one
// segment, never parsed and never escaped (thousands of keys end in ".").
//
// Every key lok prints goes through FormatKey and every key it reads through
// SplitKey, so a key copied out of grep, check, missing or a merge clash
// pastes straight back into get/set/rm. Object.Leaves keeps the raw "."-join
// for configdiscover, which compares keys to values to detect the style.
// ---------------------------------------------------------------------------

// SplitKey parses a key into the segments that address it in the catalog.
func SplitKey(style Style, key string) ([]string, error) {
	if style != StylePath {
		return []string{key}, nil
	}
	if !strings.Contains(key, `\`) {
		return strings.Split(key, "."), nil
	}
	var segs []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ { // bytewise: '.' and '\' never occur inside a UTF-8 sequence
		switch c := key[i]; c {
		case '.':
			segs = append(segs, cur.String())
			cur.Reset()
		case '\\':
			if i+1 < len(key) && (key[i+1] == '.' || key[i+1] == '\\') {
				cur.WriteByte(key[i+1])
				i++
				continue
			}
			return nil, &Diag{Code: DiagConfigInvalid, Detail: fmt.Sprintf(`bad key escape in %s at byte %d: a path key knows only \. (a literal dot) and \\ (a literal backslash)`, quoteKey(key), i)}
		default:
			cur.WriteByte(c)
		}
	}
	return append(segs, cur.String()), nil
}

// FormatKey renders segments as the canonical key: path segments holding a
// "." or "\" are escaped, everything else prints as it always did.
func FormatKey(style Style, segs []string) string {
	if style != StylePath {
		return strings.Join(segs, ".")
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		if strings.ContainsAny(s, `.\`) {
			s = strings.NewReplacer(`\`, `\\`, `.`, `\.`).Replace(s)
		}
		out[i] = s
	}
	return strings.Join(out, ".")
}

// quoteKey single-quotes a key for a message a human pastes into a shell;
// %q would double an escaped key's backslash.
func quoteKey(k string) string {
	return "'" + strings.ReplaceAll(k, "'", `'\''`) + "'"
}

// resolveLoose is the did-you-mean search behind KEY_MISSING in a path
// catalog: it splits the key on EVERY dot (escaped or not) and walks the real
// tree, joining consecutive atoms until a child matches, so a key whose
// backslash the shell ate (`codes.bankid.x.title`) or one escaped where the
// tree nests finds its leaf. It only ever feeds a diagnostic's Fix; lookups
// never resolve through it, so meaning never depends on tree contents.
// only limits the walk to those locales (nil: every locale).
func (c *Catalog) resolveLoose(key string, only []string) []string {
	if c.Config.Style != StylePath {
		return nil
	}
	var atoms []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		switch {
		case key[i] == '.':
			atoms = append(atoms, cur.String())
			cur.Reset()
		case key[i] == '\\' && i+1 < len(key) && key[i+1] == '.':
		case key[i] == '\\' && i+1 < len(key) && key[i+1] == '\\':
			cur.WriteByte('\\')
			i++
		default:
			cur.WriteByte(key[i])
		}
	}
	atoms = append(atoms, cur.String())
	seen := map[string]bool{}
	var out []string
	var walk func(node any, i int, segs []string)
	walk = func(node any, i int, segs []string) {
		if i == len(atoms) {
			if _, leaf := node.(string); leaf {
				if k := FormatKey(StylePath, segs); !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
			return
		}
		for j := i + 1; j <= len(atoms); j++ {
			seg := strings.Join(atoms[i:j], ".")
			if next, ok := child(node, seg); ok {
				walk(next, j, append(segs[:len(segs):len(segs)], seg))
			}
		}
	}
	for _, code := range c.Config.Locales {
		if len(only) == 0 || contains(only, code) {
			walk(c.Locales[code].Object, 0, nil)
		}
	}
	sort.Strings(out)
	return out
}

// dottedSiblings lists the keys of o that start with seg + ".": the
// siblings a new intermediate object named seg would silently shadow.
func (o *Object) dottedSiblings(seg string) []string {
	var out []string
	for _, e := range o.Entries {
		if strings.HasPrefix(e.Key, seg+".") {
			out = append(out, e.Key)
		}
	}
	return out
}
