package lok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellword"
)

// ---------------------------------------------------------------------------
// Ordered JSON. Catalogs keep their key order, and inserts go to the sorted
// slot without ever reordering existing keys — a diff shows only the change.
//
// Values: string (a translatable leaf), *Object, *Array, or Scalar (number,
// bool, null — preserved verbatim, never translated).
// ---------------------------------------------------------------------------

// Entry is one key/value pair of an ordered object.
type Entry struct {
	Key   string
	Value any
}

// Object is an insertion-ordered JSON object.
type Object struct{ Entries []Entry }

// Array is a JSON array; elements are addressed by numeric path segment.
type Array struct {
	Items []any
	// Inline remembers a source array written on one line (prettier keeps
	// short arrays inline), so a save never reflows arrays it did not touch.
	Inline bool
}

// Scalar is a non-string JSON leaf kept byte-for-byte.
type Scalar json.RawMessage

func (o *Object) index(key string) int {
	for i, e := range o.Entries {
		if e.Key == key {
			return i
		}
	}
	return -1
}

// Get looks up a direct child.
func (o *Object) Get(key string) (any, bool) {
	if i := o.index(key); i >= 0 {
		return o.Entries[i].Value, true
	}
	return nil, false
}

// less is the insertion order for new keys: case-insensitive, then exact.
func less(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// Set replaces an existing child in place or inserts a new one at the first
// slot whose key sorts after it (keys after that slot are left untouched even
// if they are out of order — we never reorder what we did not write).
func (o *Object) Set(key string, value any) {
	if i := o.index(key); i >= 0 {
		o.Entries[i].Value = value
		return
	}
	at := len(o.Entries)
	for i, e := range o.Entries {
		if less(key, e.Key) {
			at = i
			break
		}
	}
	o.Entries = append(o.Entries, Entry{})
	copy(o.Entries[at+1:], o.Entries[at:])
	o.Entries[at] = Entry{Key: key, Value: value}
}

// Delete removes a direct child; reports whether it existed.
func (o *Object) Delete(key string) bool {
	i := o.index(key)
	if i < 0 {
		return false
	}
	o.Entries = append(o.Entries[:i], o.Entries[i+1:]...)
	return true
}

// ParseObject decodes a JSON object preserving key order.
// parser pairs the decoder with its source so array layout can be read back.
type parser struct {
	*json.Decoder
	data []byte
}

func ParseObject(data []byte) (*Object, error) {
	dec := &parser{Decoder: json.NewDecoder(bytes.NewReader(data)), data: data}
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("catalog root is not an object")
	}
	obj, err := parseObjectBody(dec, "")
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing content after the catalog object")
	}
	return obj, nil
}

func parseValue(dec *parser, tok json.Token, path string) (any, error) {
	switch v := tok.(type) {
	case string:
		return v, nil
	case json.Delim:
		switch v {
		case '{':
			return parseObjectBody(dec, path)
		case '[':
			return parseArrayBody(dec, path)
		}
		return nil, fmt.Errorf("at %q: unexpected %v", path, v)
	case nil:
		return Scalar("null"), nil
	case bool:
		return Scalar(strconv.FormatBool(v)), nil
	case json.Number:
		return Scalar(v.String()), nil
	default:
		return nil, fmt.Errorf("at %q: unsupported value %v", path, tok)
	}
}

func parseObjectBody(dec *parser, path string) (*Object, error) {
	obj := &Object{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("at %q: non-string key %v", path, tok)
		}
		full := joinPath(path, key)
		vt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		val, err := parseValue(dec, vt, full)
		if err != nil {
			return nil, err
		}
		obj.Entries = append(obj.Entries, Entry{Key: key, Value: val})
	}
	_, err := dec.Token() // closing }
	return obj, err
}

func parseArrayBody(dec *parser, path string) (*Array, error) {
	arr := &Array{}
	start := dec.InputOffset()
	for dec.More() {
		vt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		val, err := parseValue(dec, vt, joinPath(path, strconv.Itoa(len(arr.Items))))
		if err != nil {
			return nil, err
		}
		arr.Items = append(arr.Items, val)
	}
	_, err := dec.Token() // closing ]
	arr.Inline = err == nil && !bytes.Contains(dec.data[start:dec.InputOffset()], []byte("\n"))
	return arr, err
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// Marshal renders with two-space indent and a trailing newline — the shape
// JSON.stringify(o, null, 2) + "\n" produces.
func (o *Object) Marshal() []byte {
	var b bytes.Buffer
	writeValue(&b, o, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any, depth int) {
	indent := strings.Repeat("  ", depth+1)
	switch x := v.(type) {
	case string:
		b.Write(jsonString(x))
	case Scalar:
		b.Write([]byte(x))
	case *Object:
		if len(x.Entries) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, e := range x.Entries {
			b.WriteString(indent)
			b.Write(jsonString(e.Key))
			b.WriteString(": ")
			writeValue(b, e.Value, depth+1)
			if i < len(x.Entries)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteByte('}')
	case *Array:
		if len(x.Items) == 0 {
			b.WriteString("[]")
			return
		}
		if x.Inline {
			b.WriteByte('[')
			for i, it := range x.Items {
				if i > 0 {
					b.WriteString(", ")
				}
				writeValue(b, it, depth+1)
			}
			b.WriteByte(']')
			return
		}
		b.WriteString("[\n")
		for i, it := range x.Items {
			b.WriteString(indent)
			writeValue(b, it, depth+1)
			if i < len(x.Items)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteByte(']')
	}
}

// jsonString encodes like JSON.stringify: no HTML escaping, UTF-8 kept.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Leaf is one string leaf addressed by its raw path segments.
type Leaf struct {
	Path  []string
	Value string
}

// LeafPaths lists every string leaf in file order with its segments, which
// FormatKey turns into the canonical key of the catalog's style.
func (o *Object) LeafPaths() []Leaf {
	var out []Leaf
	var walk func(any, []string)
	walk = func(v any, path []string) {
		switch x := v.(type) {
		case string:
			out = append(out, Leaf{Path: path, Value: x})
		case *Object:
			for _, e := range x.Entries {
				walk(e.Value, append(path[:len(path):len(path)], e.Key))
			}
		case *Array:
			for i, it := range x.Items {
				walk(it, append(path[:len(path):len(path)], strconv.Itoa(i)))
			}
		}
	}
	walk(o, nil)
	return out
}

// Leaves flattens to a raw "."-joined path → string value in file order;
// array elements get numeric segments. Scalars are not leaves. The join is
// NOT the key grammar (a segment holding a dot is not escaped): configdiscover
// compares these keys to their values to detect english-as-key files. Keys a
// human or a verb reads come from LeafPaths + FormatKey.
func (o *Object) Leaves() []Entry {
	var out []Entry
	var walk func(any, string)
	walk = func(v any, prefix string) {
		switch x := v.(type) {
		case string:
			out = append(out, Entry{Key: prefix, Value: x})
		case *Object:
			for _, e := range x.Entries {
				walk(e.Value, joinPath(prefix, e.Key))
			}
		case *Array:
			for i, it := range x.Items {
				walk(it, joinPath(prefix, strconv.Itoa(i)))
			}
		}
	}
	walk(o, "")
	return out
}

// child steps one path segment into an object or array.
func child(container any, seg string) (any, bool) {
	switch x := container.(type) {
	case *Object:
		return x.Get(seg)
	case *Array:
		if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(x.Items) {
			return x.Items[i], true
		}
	}
	return nil, false
}

// setChild writes one segment; arrays accept an existing index or the next one.
func setChild(container any, seg string, value any) error {
	switch x := container.(type) {
	case *Object:
		x.Set(seg, value)
		return nil
	case *Array:
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i > len(x.Items) {
			return fmt.Errorf("array index %q out of range (0..%d)", seg, len(x.Items))
		}
		if i == len(x.Items) {
			x.Items = append(x.Items, value)
		} else {
			x.Items[i] = value
		}
		return nil
	}
	return fmt.Errorf("cannot set %q on a scalar", seg)
}

func deleteChild(container any, seg string) bool {
	switch x := container.(type) {
	case *Object:
		return x.Delete(seg)
	case *Array:
		if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(x.Items) {
			x.Items = append(x.Items[:i], x.Items[i+1:]...)
			return true
		}
	}
	return false
}

// splitPath addresses nested catalogs through the key grammar (SplitKey);
// flat catalogs use the whole key.
func (c *Catalog) splitPath(key string) ([]string, error) {
	return SplitKey(c.Config.Style, key)
}

// Lookup returns the string value of key in one locale; a key that does
// not parse is simply absent.
func (c *Catalog) Lookup(locale, key string) (string, bool) {
	parts, err := c.splitPath(key)
	if err != nil {
		return "", false
	}
	return c.lookupSegs(locale, parts)
}

func (c *Catalog) lookupSegs(locale string, parts []string) (string, bool) {
	loc, ok := c.Locales[locale]
	if !ok {
		return "", false
	}
	var cur any = loc.Object
	for _, p := range parts {
		next, ok := child(cur, p)
		if !ok {
			return "", false
		}
		cur = next
	}
	s, ok := cur.(string)
	return s, ok
}

// Put sets key in one locale, creating intermediate objects for path style.
func (c *Catalog) Put(locale, key, value string) error {
	parts, err := c.splitPath(key)
	if err != nil {
		return err
	}
	return c.putSegs(locale, parts, value)
}

func (c *Catalog) putSegs(locale string, parts []string, value string) error {
	loc, ok := c.Locales[locale]
	if !ok {
		return fmt.Errorf("catalog %q has no locale %q", c.ID, locale)
	}
	key := FormatKey(c.Config.Style, parts)
	var cur any = loc.Object
	for i, p := range parts[:len(parts)-1] {
		next, ok := child(cur, p)
		if !ok {
			// A new `bankid` object beside `bankid.eid_doesnt_exist` siblings
			// is a wrong-shape write: the key meant a dotted segment.
			if o, isObj := cur.(*Object); isObj {
				if dotted := o.dottedSiblings(p); len(dotted) > 0 {
					meant := append(append(append([]string{}, parts[:i]...), p+"."+parts[i+1]), parts[i+2:]...)
					for j, d := range dotted {
						dotted[j] = FormatKey(c.Config.Style, []string{d})
					}
					if len(dotted) > 3 {
						dotted = append(dotted[:3], fmt.Sprintf("%d more", len(dotted)-3))
					}
					return &Diag{Code: DiagConfigInvalid, Detail: fmt.Sprintf("%s: %s would create %q beside the dotted siblings %s under %s; a dot inside a segment is escaped as \\. - did you mean %s?", locale, shellword.Quote(key), p, strings.Join(dotted, ", "), shellword.Quote(FormatKey(c.Config.Style, parts[:i])), shellword.Quote(FormatKey(c.Config.Style, meant)))}
				}
			}
			next = &Object{}
			if err := setChild(cur, p, next); err != nil {
				return fmt.Errorf("%s %s: %w", locale, quoteKey(key), err)
			}
		}
		switch next.(type) {
		case *Object, *Array:
		default:
			return fmt.Errorf("%s: %q is a leaf, cannot nest %s under it", locale, p, quoteKey(key))
		}
		cur = next
	}
	if existing, ok := child(cur, parts[len(parts)-1]); ok {
		switch existing.(type) {
		case *Object, *Array:
			return fmt.Errorf("%s: %s is a container, not a string leaf", locale, quoteKey(key))
		}
	}
	if err := setChild(cur, parts[len(parts)-1], value); err != nil {
		return fmt.Errorf("%s %s: %w", locale, quoteKey(key), err)
	}
	loc.Exists = true
	return nil
}

// Remove deletes the leaf key from one locale; reports whether it existed.
// A container is no key (get says KEY_MISSING), so `rm codes` never drops
// the whole `codes` subtree.
func (c *Catalog) Remove(locale, key string) bool {
	loc, ok := c.Locales[locale]
	if !ok {
		return false
	}
	parts, err := c.splitPath(key)
	if err != nil {
		return false
	}
	var cur any = loc.Object
	for _, p := range parts[:len(parts)-1] {
		next, ok := child(cur, p)
		if !ok {
			return false
		}
		cur = next
	}
	switch leaf, _ := child(cur, parts[len(parts)-1]); leaf.(type) {
	case *Object, *Array:
		return false
	}
	return deleteChild(cur, parts[len(parts)-1])
}
