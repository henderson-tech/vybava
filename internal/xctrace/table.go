// Package xctrace reads Instruments traces through `xctrace export`: the
// table of contents, any exported table as a stream of rows with id/ref
// resolved, and the hitch, render, update and time-profile readings perflab's
// analyze builds on. Every number is a pure function of the exported XML, so
// `perflab analyze --reread` recomputes it from the trace alone.
package xctrace

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Node is one element of an exported row. A cell written as `ref="7"` IS the
// node registered by `id="7"` (the same pointer): its children are never
// appended twice, and a ref'd node is never extended by a later reference.
type Node struct {
	Name     string
	ID       string
	Fmt      string
	Attrs    map[string]string
	Text     string
	Children []*Node
}

// Int reads the node's text as an integer (raw nanoseconds, counts, ids).
func (n *Node) Int() (int64, bool) {
	if n == nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(n.Text), 10, 64)
	return v, err == nil
}

// Child is the first direct child named name, or nil.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Label is the cell's display text: its fmt attribute, else its text.
func (n *Node) Label() string {
	if n == nil {
		return ""
	}
	if n.Fmt != "" {
		return n.Fmt
	}
	return strings.TrimSpace(n.Text)
}

// Schema is an exported table's name and its column mnemonics in row order.
type Schema struct {
	Name    string
	Columns []string
}

// Row is one table row: one top-level cell per schema column.
type Row struct {
	Schema *Schema
	Cells  []*Node
}

// Cell is the row's cell for a column mnemonic, or nil when the schema has
// no such column (a different Xcode, a different table).
func (r Row) Cell(mnemonic string) *Node {
	for i, c := range r.Schema.Columns {
		if c == mnemonic && i < len(r.Cells) {
			return r.Cells[i]
		}
	}
	return nil
}

// errUnresolvedRef marks an export whose ref names no earlier id: a cut or
// spliced file, never a whole export.
var errUnresolvedRef = errors.New("unresolved ref")

// ReadTable streams every <row> of one exported table to fn. The schema is
// returned even when the table has no rows; an export holding no <schema>
// (a table the trace does not have) returns an empty Schema.Name.
func ReadTable(r io.Reader, fn func(Row) error) (Schema, error) {
	dec := xml.NewDecoder(r)
	var schema Schema
	byID := map[string]*Node{}
	var (
		inSchema, inMnemonic bool
		mnemonic             strings.Builder
		inRow                bool
		row                  Row
		stack                []*Node
		refDepth             int
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return schema, nil
		}
		if err != nil {
			return schema, fmt.Errorf("export XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if !inRow {
				switch {
				case name == "schema":
					inSchema = true
					schema = Schema{Name: attr(t, "name")}
				case inSchema && name == "mnemonic":
					inMnemonic = true
					mnemonic.Reset()
				case name == "row":
					inRow = true
					row = Row{Schema: &schema}
					stack = stack[:0]
					refDepth = 0
				}
				continue
			}
			if refDepth > 0 {
				refDepth++
				continue
			}
			var node *Node
			if ref := attr(t, "ref"); ref != "" {
				node = byID[ref]
				if node == nil {
					return schema, fmt.Errorf("%w %q in a %s row", errUnresolvedRef, ref, schema.Name)
				}
				refDepth = 1
			} else {
				node = &Node{Name: name}
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "id":
						node.ID = a.Value
					case "fmt":
						node.Fmt = a.Value
					default:
						if node.Attrs == nil {
							node.Attrs = map[string]string{}
						}
						node.Attrs[a.Name.Local] = a.Value
					}
				}
				if node.ID != "" {
					byID[node.ID] = node
				}
			}
			if len(stack) == 0 {
				row.Cells = append(row.Cells, node)
			} else {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, node)
			}
			if refDepth == 0 {
				stack = append(stack, node)
			}
		case xml.EndElement:
			name := t.Name.Local
			if !inRow {
				switch name {
				case "mnemonic":
					if inMnemonic {
						schema.Columns = append(schema.Columns, strings.TrimSpace(mnemonic.String()))
					}
					inMnemonic = false
				case "schema":
					inSchema = false
				}
				continue
			}
			if refDepth > 0 {
				refDepth--
				continue
			}
			if name == "row" && len(stack) == 0 {
				inRow = false
				if err := fn(row); err != nil {
					return schema, err
				}
				continue
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			switch {
			case inMnemonic:
				mnemonic.Write(t)
			case inRow && refDepth == 0 && len(stack) > 0:
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
