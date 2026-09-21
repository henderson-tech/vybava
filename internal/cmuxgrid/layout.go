// Package cmuxgrid creates fresh monitor-sized cmux workspaces using native layouts.
package cmuxgrid

import (
	"errors"
	"fmt"
)

type Shape struct {
	Columns int `json:"columns"`
	Rows    int `json:"rows"`
}

// Monitor dimensions are macOS points, not physical Retina pixels.
func ForMonitor(width, height int, name string) (Shape, error) {
	if width <= 0 || height <= 0 {
		return Shape{}, errors.New("monitor width and height must be positive")
	}
	if height > width {
		return Shape{3, 4}, nil
	}
	if name == "Pro Display XDR" || width >= 3000 {
		return Shape{5, 2}, nil
	}
	return Shape{4, 2}, nil
}

type Surface struct {
	Type  string `json:"type"`
	Focus bool   `json:"focus,omitempty"`
}
type Pane struct {
	Surfaces []Surface `json:"surfaces"`
}
type Node struct {
	Pane      *Pane   `json:"pane,omitempty"`
	Direction string  `json:"direction,omitempty"`
	Split     float64 `json:"split,omitempty"`
	Children  []Node  `json:"children,omitempty"`
}

// Balanced splits avoid extremely small intermediate panes. Ratios are weighted
// by leaf count: five columns are 2/5 + 3/5, not five unequal repeated halves.
func Layout(shape Shape) (Node, error) {
	if shape != (Shape{4, 2}) && shape != (Shape{5, 2}) && shape != (Shape{3, 4}) {
		return Node{}, fmt.Errorf("unsupported grid %dx%d", shape.Columns, shape.Rows)
	}
	var build func(int, int, bool) Node
	build = func(cols, rows int, first bool) Node {
		if cols == 1 && rows == 1 {
			return Node{Pane: &Pane{[]Surface{{Type: "terminal", Focus: first}}}}
		}
		n, direction := cols, "horizontal"
		if cols == 1 {
			n, direction = rows, "vertical"
		}
		left := n / 2
		var a, b Node
		if cols > 1 {
			a, b = build(left, rows, first), build(n-left, rows, false)
		} else {
			a, b = build(1, left, first), build(1, n-left, false)
		}
		return Node{Direction: direction, Split: float64(left) / float64(n), Children: []Node{a, b}}
	}
	return build(shape.Columns, shape.Rows, true), nil
}
