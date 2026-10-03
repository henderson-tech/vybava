// Package journeys validates human-authored journey libraries and records evidence.
// It never interprets prose as executable commands or performs app interactions.
package journeys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"
)

type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}
type Problem struct{ Diagnostic }

func (p *Problem) Error() string { return p.Code + ": " + p.Message }
func problem(code, message string) error {
	return &Problem{Diagnostic: Diagnostic{Code: code, Message: message}}
}

type Properties struct {
	Schema      string   `yaml:"schema" json:"schema"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Type        string   `yaml:"type" json:"type"`
	Status      string   `yaml:"status" json:"status"`
	Tags        []string `yaml:"tags" json:"tags"`
	Aliases     []string `yaml:"aliases" json:"aliases"`
	Verified    string   `yaml:"last-verified" json:"lastVerified"`
	Mode        string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	Actors      []string `yaml:"actors,omitempty" json:"actors,omitempty"`
	Includes    []string `yaml:"includes,omitempty" json:"includes,omitempty"`
	Seed        string   `yaml:"seed,omitempty" json:"seed,omitempty"`
	Verify      string   `yaml:"verify,omitempty" json:"verify,omitempty"`
	Story       string   `yaml:"story,omitempty" json:"story,omitempty"`
}
type Document struct {
	Properties
	Path     string    `json:"path"`
	Hash     string    `json:"hash"`
	Body     []byte    `json:"-"`
	Front    yaml.Node `json:"-"`
	Original []byte    `json:"-"`
	BodyLine int       `json:"-"`
}
type Scenario struct {
	ID       string   `json:"id"`
	Modes    []string `json:"modes"`
	Moment   string   `json:"moment"`
	Intent   string   `json:"intent"`
	Expected string   `json:"expected"`
	Path     string   `json:"path"`
}
type Library struct {
	Root        string       `json:"-"`
	Dir         string       `json:"-"`
	Documents   []*Document  `json:"documents"`
	Scenarios   []Scenario   `json:"scenarios"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Hash        string       `json:"hash"`
	names       map[string]*Document
	paths       map[string]*Document
}

var slug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var story = regexp.MustCompile(`^[a-z0-9-]+/[1-9][0-9]*$`)
var wiki = regexp.MustCompile(`\[\[([^\]\n]+)\]\]`)
var step = regexp.MustCompile(`\*\*([A-Z][A-Z0-9]*-[0-9]{2,})\b`)
var keys = []string{"schema", "name", "description", "type", "status", "tags", "aliases", "last-verified", "mode", "actors", "includes", "seed", "verify", "story"}

func Digest(b []byte) string     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ModeKey(mode string) string { return strings.ReplaceAll(strings.ToLower(mode), "_", "-") }

// SafePath refuses traversal and symlink escapes, including an intermediate link.
func SafePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return "", problem("TARGET_UNSAFE", "path must be relative")
	}
	for _, p := range strings.Split(relative, "/") {
		if p == ".." {
			return "", problem("TARGET_UNSAFE", "parent traversal is forbidden")
		}
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(r, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", problem("TARGET_UNSAFE", "symlink escapes root")
	}
	return resolved, nil
}
func Parse(path string, b []byte) (*Document, error) {
	normal := bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normal, []byte("---\n")) {
		return nil, problem("DOCUMENT_INVALID", "frontmatter is required")
	}
	end := bytes.Index(normal[4:], []byte("\n---\n"))
	if end < 0 {
		return nil, problem("DOCUMENT_INVALID", "unterminated frontmatter")
	}
	end += 4
	d := &Document{Path: path, Original: b, Body: normal[end+5:], Hash: Digest(b), BodyLine: bytes.Count(normal[:end+5], []byte("\n")) + 1}
	if err := yaml.Unmarshal(normal[4:end], &d.Front); err != nil {
		return nil, problem("DOCUMENT_INVALID", "malformed YAML")
	}
	if len(d.Front.Content) != 1 || d.Front.Content[0].Kind != yaml.MappingNode {
		return nil, problem("DOCUMENT_INVALID", "flat mapping required")
	}
	seen := map[string]bool{}
	for i := 0; i < len(d.Front.Content[0].Content); i += 2 {
		k, v := d.Front.Content[0].Content[i], d.Front.Content[0].Content[i+1]
		if seen[k.Value] || (!slices.Contains(keys, k.Value) && !strings.HasPrefix(k.Value, "x-")) {
			return nil, problem("DOCUMENT_INVALID", "duplicate or unknown property at line "+fmt.Sprint(k.Line+1))
		}
		seen[k.Value] = true
		if v.Kind != yaml.ScalarNode && v.Kind != yaml.SequenceNode {
			return nil, problem("DOCUMENT_INVALID", "nested properties are forbidden")
		}
		if v.Kind == yaml.SequenceNode {
			for _, n := range v.Content {
				if n.Kind != yaml.ScalarNode {
					return nil, problem("DOCUMENT_INVALID", "flat lists required")
				}
			}
		}
	}
	if err := d.Front.Decode(&d.Properties); err != nil {
		return nil, problem("DOCUMENT_INVALID", "invalid property types")
	}
	for _, k := range keys[:8] {
		if !seen[k] {
			return nil, problem("DOCUMENT_INVALID", "missing property: "+k)
		}
	}
	if d.Schema != "user-journeys/v1" || !slug.MatchString(d.Name) || strings.TrimSpace(d.Description) == "" || !slices.Contains([]string{"index", "journey", "reference", "seed", "verification"}, d.Type) || !slices.Contains([]string{"draft", "observed", "retired"}, d.Status) {
		return nil, problem("DOCUMENT_INVALID", "invalid schema, name, description, type or status")
	}
	if _, err := time.Parse("2006-01-02", d.Verified); err != nil {
		return nil, problem("DOCUMENT_INVALID", "last-verified must be an ISO date")
	}
	if d.Type == "journey" && (d.Mode == "" || len(d.Actors) < 2 || len(d.Includes) == 0 || d.Seed == "" || d.Verify == "" || !story.MatchString(d.Story)) {
		return nil, problem("DOCUMENT_INVALID", "journey needs mode, actors, includes, seed, verify and qualified story")
	}
	if len(secretscan.Find(string(b), secretscan.All, nil)) > 0 {
		return nil, problem("DOCUMENT_INVALID", "secret hygiene check failed; content withheld")
	}
	return d, nil
}

func Load(root, dir string) (*Library, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// SafePath returns canonical paths. Keep the walk's root in the same
	// namespace, including when a checkout or a system temp directory is linked.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	base, err := SafePath(root, dir)
	if err != nil {
		return nil, err
	}
	l := &Library{Root: root, Dir: dir, names: map[string]*Document{}, paths: map[string]*Document{}, Diagnostics: []Diagnostic{}, Scenarios: []Scenario{}}
	err = filepath.WalkDir(base, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".md" || filepath.Base(p) == "INDEX.md" || strings.HasPrefix(p, filepath.Join(base, "runs")+string(filepath.Separator)) && filepath.Base(p) != "README.md" {
			return nil
		}
		rel, e2 := filepath.Rel(root, p)
		if e2 != nil {
			return e2
		}
		rel = filepath.ToSlash(rel)
		safe, e2 := SafePath(root, rel)
		if e2 != nil {
			l.add("TARGET_UNSAFE", rel, 0, "unsafe document path")
			return nil
		}
		b, e2 := os.ReadFile(safe)
		if e2 != nil {
			return e2
		}
		d, e2 := Parse(rel, b)
		if e2 != nil {
			l.add("DOCUMENT_INVALID", rel, 1, e2.Error())
			return nil
		}
		if _, ok := l.names[d.Name]; ok {
			l.add("DOCUMENT_INVALID", rel, 1, "duplicate name: "+d.Name)
		}
		l.Documents = append(l.Documents, d)
		l.names[d.Name] = d
		l.paths[rel] = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(l.Documents) == 0 {
		l.add("DOCUMENT_INVALID", dir, 0, "empty library")
	}
	var hashes strings.Builder
	modes := map[string]bool{}
	scenarioIDs := map[string]bool{}
	steps := map[string]bool{}
	for _, d := range l.Documents {
		hashes.WriteString(d.Path + ":" + d.Hash + "\n")
		if d.Type == "journey" {
			k := ModeKey(d.Mode)
			if !slug.MatchString(k) || modes[k] {
				l.add("DOCUMENT_INVALID", d.Path, 1, "invalid or duplicate journey mode")
			}
			modes[k] = true
		}
	}
	l.Hash = Digest([]byte(hashes.String()))
	for _, d := range l.Documents {
		l.links(d)
		for _, pair := range []struct{ p, t string }{{d.Seed, "seed"}, {d.Verify, "verification"}} {
			if pair.p != "" {
				p := filepath.ToSlash(filepath.Join(dir, pair.p))
				target := l.paths[p]
				if target == nil || target.Type != pair.t {
					l.add("LINK_UNRESOLVED", d.Path, 1, "invalid "+pair.t+" reference")
				}
			}
		}
		for _, m := range step.FindAllSubmatch(d.Body, -1) {
			id := string(m[1])
			if steps[id] {
				l.add("DOCUMENT_INVALID", d.Path, 0, "duplicate step ID: "+id)
			}
			steps[id] = true
		}
		l.tables(d, modes, scenarioIDs)
		if _, err := l.Compose(d.Name); err != nil {
			l.add("COMPOSITION_CYCLE", d.Path, 1, err.Error())
		}
	}
	return l, nil
}
func (l *Library) add(code, path string, line int, message string) {
	l.Diagnostics = append(l.Diagnostics, Diagnostic{code, path, line, message})
}
func (l *Library) Valid() error {
	if len(l.Diagnostics) > 0 {
		return problem("DOCUMENT_INVALID", fmt.Sprintf("%d library diagnostics", len(l.Diagnostics)))
	}
	return nil
}
func (l *Library) Compose(name string) ([]string, error) {
	visiting, done := map[string]bool{}, map[string]bool{}
	out := []string{}
	var visit func(string) error
	visit = func(n string) error {
		if visiting[n] {
			return problem("COMPOSITION_CYCLE", "include cycle at "+n)
		}
		if done[n] {
			return nil
		}
		d := l.names[n]
		if d == nil {
			return problem("LINK_UNRESOLVED", "unknown include "+n)
		}
		visiting[n] = true
		for _, i := range d.Includes {
			if err := visit(i); err != nil {
				return err
			}
		}
		delete(visiting, n)
		done[n] = true
		out = append(out, d.Path)
		return nil
	}
	err := visit(name)
	return out, err
}
func markdown(b []byte) ast.Node {
	return goldmark.New(goldmark.WithExtensions(extension.Table)).Parser().Parse(text.NewReader(b))
}

// Mask code AST ranges so illustrative links in fenced or inline code are inert.
func prose(b []byte) []byte {
	out := bytes.Clone(b)
	_ = ast.Walk(markdown(b), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindFencedCodeBlock, ast.KindCodeBlock:
			for i := 0; i < n.Lines().Len(); i++ {
				s := n.Lines().At(i)
				for j := s.Start; j < s.Stop; j++ {
					out[j] = ' '
				}
			}
			return ast.WalkSkipChildren, nil
		case ast.KindCodeSpan:
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					for j := t.Segment.Start; j < t.Segment.Stop; j++ {
						out[j] = ' '
					}
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return out
}
func (l *Library) links(d *Document) {
	p := prose(d.Body)
	for _, m := range wiki.FindAllSubmatchIndex(p, -1) {
		raw := string(p[m[2]:m[3]])
		target := strings.SplitN(raw, "|", 2)[0]
		parts := strings.SplitN(target, "#", 2)
		path := parts[0]
		if path == "" {
			path = d.Path
		} else if filepath.Ext(path) == "" {
			path += ".md"
		}
		line := d.BodyLine + bytes.Count(p[:m[0]], []byte("\n"))
		resolved, err := SafePath(l.Root, path)
		if err != nil {
			l.add("LINK_UNRESOLVED", d.Path, line, "missing or unsafe wikilink target")
			continue
		}
		if len(parts) == 2 {
			b, err := os.ReadFile(resolved)
			if err != nil {
				l.add("LINK_UNRESOLVED", d.Path, line, "cannot read anchor target")
				continue
			}
			anchor := parts[1]
			found := false
			if strings.HasPrefix(anchor, "^") {
				found = regexp.MustCompile(`(?m)(?:^|\s)` + regexp.QuoteMeta(anchor) + `\s*$`).Match(prose(b))
			} else {
				_ = ast.Walk(markdown(b), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
					if enter && n.Kind() == ast.KindHeading && strings.EqualFold(strings.TrimSpace(string(n.Text(b))), anchor) {
						found = true
					}
					return ast.WalkContinue, nil
				})
			}
			if !found {
				l.add("LINK_UNRESOLVED", d.Path, line, "missing heading or block anchor")
			}
		}
	}
}
func (l *Library) tables(d *Document, modes, seen map[string]bool) {
	_ = ast.Walk(markdown(d.Body), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter || n.Kind() != extast.KindTable {
			return ast.WalkContinue, nil
		}
		header := n.FirstChild()
		if header == nil {
			return ast.WalkContinue, nil
		}
		cells := func(row ast.Node) []string {
			v := []string{}
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				v = append(v, strings.TrimSpace(strings.ReplaceAll(string(c.Text(d.Body)), `\|`, `|`)))
			}
			return v
		}
		h := cells(header)
		if len(h) == 0 || h[0] != "ID" {
			return ast.WalkSkipChildren, nil
		}
		if !slices.Equal(h, []string{"ID", "Modes", "Moment", "Person's intent or event", "Expected observation"}) {
			l.add("DOCUMENT_INVALID", d.Path, 0, "invalid scenario table header")
			return ast.WalkSkipChildren, nil
		}
		for row := header.NextSibling(); row != nil; row = row.NextSibling() {
			v := cells(row)
			if len(v) != 5 {
				l.add("DOCUMENT_INVALID", d.Path, 0, "scenario requires five cells")
				continue
			}
			invalid := false
			for _, s := range v {
				invalid = invalid || s == ""
			}
			if invalid || seen[v[0]] {
				l.add("DOCUMENT_INVALID", d.Path, 0, "empty scenario field or duplicate ID")
				continue
			}
			seen[v[0]] = true
			ms := strings.Fields(v[1])
			if v[1] == "all" {
				ms = nil
				for mode := range modes {
					ms = append(ms, mode)
				}
				sort.Strings(ms)
			}
			unique := map[string]bool{}
			for _, mode := range ms {
				if !modes[mode] || unique[mode] {
					l.add("DOCUMENT_INVALID", d.Path, 0, "unknown or duplicate scenario mode")
				}
				unique[mode] = true
			}
			l.Scenarios = append(l.Scenarios, Scenario{v[0], ms, v[2], v[3], v[4], d.Path})
		}
		return ast.WalkSkipChildren, nil
	})
}
func Format(d *Document) ([]byte, error) {
	mapping := d.Front.Content[0]
	ordered := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	order := append([]string{}, keys...)
	for i := 0; i < len(mapping.Content); i += 2 {
		if strings.HasPrefix(mapping.Content[i].Value, "x-") {
			order = append(order, mapping.Content[i].Value)
		}
	}
	sort.Strings(order[len(keys):])
	for _, key := range order {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value != key {
				continue
			}
			v := *mapping.Content[i+1]
			if v.Kind == yaml.SequenceNode {
				v.Style = yaml.FlowStyle
			}
			if key == "last-verified" {
				v.Tag = "!!str"
				v.Style = yaml.DoubleQuotedStyle
			}
			ordered.Content = append(ordered.Content, mapping.Content[i], &v)
		}
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(ordered); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	b.WriteString("---\n")
	b.Write(d.Body)
	return b.Bytes(), nil
}
func (l *Library) Index() []byte {
	var b strings.Builder
	b.WriteString("<!-- generated by journeys index; do not edit -->\n# Journey index\n\n| Name | Type | Mode | Source |\n|---|---|---|---|\n")
	for _, d := range l.Documents {
		fmt.Fprintf(&b, "| %s | %s | %s | [[%s]] |\n", d.Name, d.Type, d.Mode, strings.TrimSuffix(d.Path, ".md"))
	}
	fmt.Fprintf(&b, "\nAuthored scenarios: %d. Candidate mode cells: %d. This index claims no execution.\n", len(l.Scenarios), l.CandidateCount())
	return []byte(b.String())
}
func (l *Library) CandidateCount() int {
	n := 0
	for _, s := range l.Scenarios {
		n += len(s.Modes)
	}
	return n
}
