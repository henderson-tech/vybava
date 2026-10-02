package buildindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KeyVersion prefixes every portable native key; bump it when the recipe
// below changes, so an old index entry can never answer a new lookup.
const KeyVersion = "pf1"

// The Expo fingerprint alone cannot key a native build: it hashes
// `../`-relative paths, so the same content at another checkout depth (main
// vs .worktrees/<slug>, bun's isolated links under ~/.bun) hashes
// differently, and it misses inputs Expo does not know (Reanimated's
// staticFeatureFlags). The portable key re-derives a depth-free line set:
//
//	file\t<normalized path>\t<hash>        every leaf of every file/dir source
//	dir\t<normalized path>\tnull           a dir Expo could not read (symlinked links)
//	contents\t<id>\t<sha256(normalized)>   contents sources, embedded paths normalized
//	extra\t<file>#<pointer>\t<sha256(JSON)> adapter extraNativeInputs
//	platform|profile|kind|signing|toolchain rows
//
// key = KeyVersion + "-" + sha256(sorted lines)[:20]. SourceKey hashes only
// the source, extra, platform and profile rows: it is what a JS bundle and a
// native build must share for the bundle to run in it (pack's
// FINGERPRINT_MISMATCH gate), independent of kind, signing and toolchain.

// Signing is the signing input of a native key.
type Signing struct {
	Team               string `json:"team,omitempty"`
	IdentitySHA1       string `json:"identitySha1,omitempty"`
	KeystoreCertSHA256 string `json:"keystoreCertSha256,omitempty"`
}

// Toolchain is the build-tool input of a native key.
type Toolchain struct {
	Xcode string `json:"xcode,omitempty"`
	JDK   string `json:"jdk,omitempty"`
}

// KeyInputs is everything ComputeKey needs besides the fingerprint JSON.
type KeyInputs struct {
	Platform  string
	Profile   string
	Kind      string
	Signing   Signing
	Toolchain Toolchain
	// Extras are the resolved extraNativeInputs (ReadExtraInputs).
	Extras []ExtraInput
	// Roots are absolute prefixes stripped from absolute paths, longest
	// first: the app root, the repo root, the home dir.
	Roots []string
}

// ExtraInput is one adapter extraNativeInputs entry, `<file>#<pointer>`.
type ExtraInput struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

// SourceCounts summarises what the key covered.
type SourceCounts struct {
	Files    int `json:"files"`
	Dirs     int `json:"dirs"`
	Leaves   int `json:"leaves"`
	NullDirs int `json:"nullDirs"`
	Contents int `json:"contents"`
	Skipped  int `json:"skipped"`
}

// Fingerprint is the `perflab fingerprint` payload.
type Fingerprint struct {
	Key         string       `json:"key"`
	SourceKey   string       `json:"sourceKey"`
	ExpoHash    string       `json:"expoHash"`
	Platform    string       `json:"platform"`
	Profile     string       `json:"profile"`
	Kind        string       `json:"kind"`
	Sources     SourceCounts `json:"sources"`
	ExtraInputs []ExtraInput `json:"extraInputs"`
	Signing     Signing      `json:"signing"`
	Toolchain   Toolchain    `json:"toolchain"`
	// AppVersion is the app config's `version` (the expoConfig source): the
	// marketing version a build of these inputs carries. Import compares it.
	AppVersion string `json:"appVersion,omitempty"`
	// Lines are the sorted key lines; the index stores them beside each
	// build so two keys can be diffed when a lookup misses.
	Lines []string `json:"-"`
}

type fpDoc struct {
	Hash    string     `json:"hash"`
	Sources []fpSource `json:"sources"`
}

type fpSource struct {
	Type      string   `json:"type"`
	FilePath  string   `json:"filePath"`
	ID        string   `json:"id"`
	Contents  *string  `json:"contents"`
	Hash      *string  `json:"hash"`
	Reasons   []string `json:"reasons"`
	DebugInfo *fpDebug `json:"debugInfo"`
}

type fpDebug struct {
	Path     string    `json:"path"`
	Hash     *string   `json:"hash"`
	Children []fpDebug `json:"children"`
}

var dotdotRun = regexp.MustCompile(`(?:\.\./)+`)

// normalizePath strips the leading ../ run and any root prefix.
func normalizePath(p string, roots []string) string {
	p = filepath.ToSlash(p)
	if strings.HasPrefix(p, "/") {
		for _, r := range roots {
			r = strings.TrimSuffix(filepath.ToSlash(r), "/") + "/"
			if strings.HasPrefix(p, r) {
				return strings.TrimPrefix(p, r)
			}
		}
		return p
	}
	p = strings.TrimPrefix(p, "./")
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return p
}

// normalizeContents removes every ../ run and root prefix from a contents
// source (autolinking configs embed podspec and root paths).
func normalizeContents(s string, roots []string) string {
	for _, r := range roots {
		r = strings.TrimSuffix(filepath.ToSlash(r), "/") + "/"
		if r != "/" {
			s = strings.ReplaceAll(s, r, "")
		}
	}
	return dotdotRun.ReplaceAllString(s, "")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ComputeKey derives the portable key from `fingerprint:generate --debug`
// output. It is a pure function of its inputs.
func ComputeKey(raw []byte, in KeyInputs) (Fingerprint, error) {
	var doc fpDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Hash == "" {
		detail := "the fingerprint output is not fingerprint JSON"
		if err != nil {
			detail += ": " + err.Error()
		}
		return Fingerprint{}, diag(DiagFingerprintFailed, detail, "bun install (the app's dependencies), then re-run perflab fingerprint")
	}
	roots := append([]string(nil), in.Roots...)
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })

	lines := map[string]bool{}
	var counts SourceCounts
	var appVersion string
	for _, s := range doc.Sources {
		switch s.Type {
		case "file":
			counts.Files++
			lines["file\t"+normalizePath(s.FilePath, roots)+"\t"+deref(s.Hash)] = true
		case "dir":
			counts.Dirs++
			if s.Hash == nil {
				if hasReason(s.Reasons, "bareNativeDir") {
					counts.Skipped++
					continue
				}
				counts.NullDirs++
				lines["dir\t"+normalizePath(s.FilePath, roots)+"\tnull"] = true
				continue
			}
			if s.DebugInfo == nil {
				return Fingerprint{}, diag(DiagFingerprintFailed,
					"dir source "+s.FilePath+" carries no debugInfo, so its leaves cannot be re-rooted",
					"run the fingerprint command with --debug (the adapter's fingerprint.cmd)")
			}
			counts.Leaves += walkLeaves(*s.DebugInfo, roots, lines)
		case "contents":
			counts.Contents++
			sum := deref(s.Hash)
			if s.Contents != nil {
				sum = sha256Hex([]byte(normalizeContents(*s.Contents, roots)))
				if s.ID == "expoConfig" {
					var cfg struct {
						Version string `json:"version"`
					}
					if json.Unmarshal([]byte(*s.Contents), &cfg) == nil {
						appVersion = cfg.Version
					}
				}
			}
			lines["contents\t"+s.ID+"\t"+sum] = true
		default:
			return Fingerprint{}, diag(DiagFingerprintFailed,
				fmt.Sprintf("unknown fingerprint source type %q; perflab %s does not know it", s.Type, KeyVersion),
				"upgrade vybava (vybava update), or pin the app's @expo/fingerprint")
		}
	}
	extras := append([]ExtraInput(nil), in.Extras...)
	sort.Slice(extras, func(i, j int) bool { return extras[i].Ref < extras[j].Ref })
	for _, e := range extras {
		lines["extra\t"+e.Ref+"\t"+e.SHA256] = true
	}
	lines["platform\t"+in.Platform] = true
	lines["profile\t"+in.Profile] = true
	sourceLines := sortedKeys(lines)

	lines["kind\t"+in.Kind] = true
	addIf(lines, "signing\tteam\t", in.Signing.Team)
	addIf(lines, "signing\tidentitySha1\t", in.Signing.IdentitySHA1)
	addIf(lines, "signing\tkeystoreCertSha256\t", in.Signing.KeystoreCertSHA256)
	addIf(lines, "toolchain\txcode\t", in.Toolchain.Xcode)
	addIf(lines, "toolchain\tjdk\t", in.Toolchain.JDK)
	all := sortedKeys(lines)

	if extras == nil {
		extras = []ExtraInput{}
	}
	return Fingerprint{
		Key:         KeyVersion + "-" + sha256Hex([]byte(strings.Join(all, "\n")))[:20],
		SourceKey:   KeyVersion + "s-" + sha256Hex([]byte(strings.Join(sourceLines, "\n")))[:20],
		ExpoHash:    doc.Hash,
		Platform:    in.Platform,
		Profile:     in.Profile,
		Kind:        in.Kind,
		Sources:     counts,
		ExtraInputs: extras,
		Signing:     in.Signing,
		Toolchain:   in.Toolchain,
		AppVersion:  appVersion,
		Lines:       all,
	}, nil
}

func walkLeaves(n fpDebug, roots []string, lines map[string]bool) int {
	if n.Children == nil {
		lines["file\t"+normalizePath(n.Path, roots)+"\t"+deref(n.Hash)] = true
		return 1
	}
	count := 0
	for _, c := range n.Children {
		count += walkLeaves(c, roots, lines)
	}
	return count
}

func deref(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

func addIf(lines map[string]bool, prefix, v string) {
	if v != "" {
		lines[prefix+v] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadExtraInputs resolves `<file>#<json pointer>` refs against appRoot to
// the sha256 of the pointed value's canonical JSON (a missing pointer
// hashes as null, so removing a flag block changes the key too).
func ReadExtraInputs(appRoot string, refs []string) ([]ExtraInput, error) {
	out := make([]ExtraInput, 0, len(refs))
	for _, ref := range refs {
		file, pointer, _ := strings.Cut(ref, "#")
		b, err := os.ReadFile(filepath.Join(appRoot, file))
		if err != nil {
			return nil, diag(DiagFingerprintFailed,
				fmt.Sprintf("extraNativeInputs %q: %v", ref, err),
				"fix perflab.fingerprint.extraNativeInputs in vybava.config.ts (paths are relative to app.root)")
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, diag(DiagFingerprintFailed,
				fmt.Sprintf("extraNativeInputs %q: %s is not JSON: %v", ref, file, err),
				"point extraNativeInputs at a JSON file")
		}
		v := jsonPointer(doc, pointer)
		canon, err := json.Marshal(v) // map keys sort: canonical
		if err != nil {
			return nil, err
		}
		out = append(out, ExtraInput{Ref: ref, SHA256: sha256Hex(canon)})
	}
	return out, nil
}

// jsonPointer resolves an RFC 6901 pointer; a missing path is nil.
func jsonPointer(doc any, pointer string) any {
	if pointer == "" || pointer == "/" {
		return doc
	}
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil
			}
			cur = next
		case []any:
			var i int
			if _, err := fmt.Sscanf(tok, "%d", &i); err != nil || i < 0 || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
	}
	return cur
}

// FingerprintSpec is one `perflab fingerprint` invocation.
type FingerprintSpec struct {
	Project Project
	Target  Target
	// Env is the profile env (KEY=VALUE); EXPO_NO_DOTENV=1 is always added.
	Env []string
	// Signing and Toolchain, when empty, are resolved from the host.
	Signing   *Signing
	Toolchain *Toolchain
	Timeout   time.Duration
}

// RunFingerprint runs the adapter's fingerprint command under the profile
// env and computes the portable key.
func RunFingerprint(ctx context.Context, r Runner, spec FingerprintSpec) (Fingerprint, error) {
	p, t := spec.Project, spec.Target
	if err := t.validate(); err != nil {
		return Fingerprint{}, err
	}
	argv := p.FingerprintCmd
	if len(argv) == 0 {
		argv = []string{"bunx", "@expo/fingerprint", "fingerprint:generate", p.AppRoot, "--platform", t.Platform, "--debug"}
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	res, err := run(ctx, r, Cmd{
		Argv:    argv,
		Dir:     p.RepoRoot,
		Env:     append([]string{"EXPO_NO_DOTENV=1"}, spec.Env...),
		Timeout: timeout,
	})
	if err != nil {
		return Fingerprint{}, err
	}
	if res.Exit != 0 {
		return Fingerprint{}, diag(DiagFingerprintFailed,
			fmt.Sprintf("`%s` exited %d: %s", strings.Join(argv, " "), res.Exit, lastLines(res.Stderr, 3)),
			"bun install (the app's dependencies), then re-run perflab fingerprint")
	}
	extras, err := ReadExtraInputs(p.appRootAbs(), p.ExtraNativeInputs)
	if err != nil {
		return Fingerprint{}, err
	}
	signing := Signing{}
	if spec.Signing != nil {
		signing = *spec.Signing
	} else if signing, err = ResolveSigning(ctx, r, p, t.Platform); err != nil {
		return Fingerprint{}, err
	}
	toolchain := Toolchain{}
	if spec.Toolchain != nil {
		toolchain = *spec.Toolchain
	} else if toolchain, err = ResolveToolchain(ctx, r, t.Platform); err != nil {
		return Fingerprint{}, err
	}
	home, _ := os.UserHomeDir()
	return ComputeKey(res.Stdout, KeyInputs{
		Platform:  t.Platform,
		Profile:   t.Profile,
		Kind:      t.Kind,
		Signing:   signing,
		Toolchain: toolchain,
		Extras:    extras,
		Roots:     []string{p.appRootAbs(), p.RepoRoot, home},
	})
}
