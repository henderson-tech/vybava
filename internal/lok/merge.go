package lok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Three-way catalog merge — the `lok merge-driver` git calls when both sides
// of a merge touched a declared catalog. Keys merge independently, so the
// line-level collisions git's text merge produces (two branches appending
// keys at the same spot) never happen; only a key both sides changed
// differently is a real clash.
//
// Result order = theirs (the branch being merged in, usually main), with
// ours' surviving additions inserted through Object.Set — the slot `lok add`
// would have picked had the key been added on top of theirs. The merged file
// therefore diffs against theirs by exactly ours' changes.
// ---------------------------------------------------------------------------

// Prefer settles clashes toward one side; PreferNone leaves them open.
type Prefer string

const (
	PreferNone   Prefer = ""
	PreferOurs   Prefer = "ours"
	PreferTheirs Prefer = "theirs"
)

// Clash is one key both sides changed to different values (or one side
// deleted while the other changed it). Values are compact JSON; "" = absent.
type Clash struct {
	Key    string `json:"key"`
	Base   string `json:"base"`
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}

// ErrNotCanonical — theirs does not round-trip byte-for-byte through lok's
// writer, so a merged write would reformat the file; the caller falls back
// to git's text merge.
var ErrNotCanonical = errors.New("catalog is not in lok's canonical format (2-space JSON); a merged write would reformat it")

// ErrDuplicateKey — a side already holds a key twice (typically left by an
// earlier line merge); the driver forces a conflict rather than carry it on.
var ErrDuplicateKey = errors.New("duplicate key")

// MergeCatalog merges three versions of one catalog file. An empty base is
// an empty catalog (git passes an empty file when both sides added it).
// With PreferNone and clashes present, Data is nil: the file is a conflict.
func MergeCatalog(base, ours, theirs []byte, prefer Prefer) (data []byte, clashes []Clash, err error) {
	parse := func(side string, b []byte) (*Object, error) {
		if len(bytes.TrimSpace(b)) == 0 {
			return &Object{}, nil
		}
		o, err := ParseObject(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", side, err)
		}
		// A key merge over duplicates would see only the first copy (Get)
		// while writing both back; the earlier merge that made them is the
		// conflict to settle first.
		if dup := duplicateKey(o, ""); dup != "" {
			return nil, fmt.Errorf("%s: %w %q", side, ErrDuplicateKey, dup)
		}
		return o, nil
	}
	b, err := parse("base", base)
	if err != nil {
		return nil, nil, err
	}
	o, err := parse("ours", ours)
	if err != nil {
		return nil, nil, err
	}
	t, err := parse("theirs", theirs)
	if err != nil {
		return nil, nil, err
	}
	trailingNewline := len(theirs) == 0 || bytes.HasSuffix(theirs, []byte("\n"))
	render := func(obj *Object) []byte {
		out := obj.Marshal()
		if !trailingNewline {
			out = bytes.TrimSuffix(out, []byte("\n"))
		}
		return out
	}
	if len(theirs) > 0 && !bytes.Equal(render(t), theirs) {
		return nil, nil, ErrNotCanonical
	}
	merged := mergeObjects("", b, o, t, prefer, &clashes)
	if len(clashes) > 0 && prefer == PreferNone {
		return nil, clashes, nil
	}
	return render(merged), clashes, nil
}

// mergeObjects merges one object level; base may be nil (absent).
func mergeObjects(path string, base, ours, theirs *Object, prefer Prefer, clashes *[]Clash) *Object {
	if base == nil {
		base = &Object{}
	}
	out := &Object{}
	for _, e := range theirs.Entries {
		bv, bok := base.Get(e.Key)
		ov, ook := ours.Get(e.Key)
		if v, keep := mergeValue(joinPath(path, e.Key), bv, bok, ov, ook, e.Value, true, prefer, clashes); keep {
			out.Entries = append(out.Entries, Entry{Key: e.Key, Value: v})
		}
	}
	for _, e := range ours.Entries {
		if _, inTheirs := theirs.Get(e.Key); inTheirs {
			continue
		}
		bv, bok := base.Get(e.Key)
		if v, keep := mergeValue(joinPath(path, e.Key), bv, bok, e.Value, true, nil, false, prefer, clashes); keep {
			out.Set(e.Key, v)
		}
	}
	return out
}

// mergeValue decides one key: equal sides agree, an unchanged side yields to
// the changed one, two objects recurse, anything else is a clash.
func mergeValue(path string, b any, bok bool, o any, ook bool, t any, tok bool, prefer Prefer, clashes *[]Clash) (any, bool) {
	same := func(x any, xok bool, y any, yok bool) bool {
		return xok == yok && (!xok || equalValue(x, y))
	}
	switch {
	case same(o, ook, t, tok):
		return o, ook
	case same(o, ook, b, bok):
		return t, tok
	case same(t, tok, b, bok):
		return o, ook
	}
	oo, oIsObj := o.(*Object)
	to, tIsObj := t.(*Object)
	bo, bIsObj := b.(*Object)
	if oIsObj && tIsObj && (bIsObj || !bok) {
		if !bIsObj {
			bo = nil
		}
		return mergeObjects(path, bo, oo, to, prefer, clashes), true
	}
	*clashes = append(*clashes, Clash{Key: path, Base: compact(b, bok), Ours: compact(o, ook), Theirs: compact(t, tok)})
	if prefer == PreferTheirs {
		return t, tok
	}
	return o, ook
}

func equalValue(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case Scalar:
		y, ok := b.(Scalar)
		return ok && bytes.Equal(x, y)
	case *Object:
		y, ok := b.(*Object)
		if !ok || len(x.Entries) != len(y.Entries) {
			return false
		}
		for i, e := range x.Entries {
			if e.Key != y.Entries[i].Key || !equalValue(e.Value, y.Entries[i].Value) {
				return false
			}
		}
		return true
	case *Array:
		y, ok := b.(*Array)
		if !ok || len(x.Items) != len(y.Items) {
			return false
		}
		for i := range x.Items {
			if !equalValue(x.Items[i], y.Items[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// compact renders a value on one line for a clash report; "" = absent.
func compact(v any, ok bool) string {
	if !ok {
		return ""
	}
	var b, out bytes.Buffer
	writeValue(&b, v, 0)
	if err := json.Compact(&out, b.Bytes()); err != nil {
		return b.String()
	}
	return out.String()
}

// duplicateKey returns the dotted path of the first key an object level
// holds twice, "" when every level is unique.
func duplicateKey(o *Object, path string) string {
	seen := map[string]bool{}
	for _, e := range o.Entries {
		if seen[e.Key] {
			return joinPath(path, e.Key)
		}
		seen[e.Key] = true
		if child, ok := e.Value.(*Object); ok {
			if dup := duplicateKey(child, joinPath(path, e.Key)); dup != "" {
				return dup
			}
		}
	}
	return ""
}
