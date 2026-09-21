package cmuxgrid

import (
	"math"
	"testing"
)

func TestMonitorShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		want Shape
	}{
		{"Built-in Retina Display", 2056, 1290, Shape{4, 2}},
		{"Pro Display XDR", 2560, 1440, Shape{5, 2}},
		{"Wide", 3008, 1692, Shape{5, 2}},
		{"Studio Display", 1440, 2560, Shape{3, 4}},
		{"Pro Display XDR", 1692, 3008, Shape{3, 4}},
	} {
		got, err := ForMonitor(tc.w, tc.h, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := ForMonitor(0, 100, ""); err == nil {
		t.Fatal("accepted unknown monitor dimensions")
	}
}

func TestLayoutsHaveEqualCellsAndOneFocusedTerminal(t *testing.T) {
	for _, shape := range []Shape{{4, 2}, {5, 2}, {3, 4}} {
		root, err := Layout(shape)
		if err != nil {
			t.Fatal(err)
		}
		count, focused := 0, 0
		var visit func(Node, float64, float64)
		visit = func(n Node, w, h float64) {
			if n.Pane != nil {
				count++
				if len(n.Pane.Surfaces) != 1 || n.Pane.Surfaces[0].Type != "terminal" {
					t.Fatal("not one terminal per cell")
				}
				if n.Pane.Surfaces[0].Focus {
					focused++
				}
				if math.Abs(w-1/float64(shape.Columns)) > 1e-9 || math.Abs(h-1/float64(shape.Rows)) > 1e-9 {
					t.Fatalf("%v: unequal cell %f × %f", shape, w, h)
				}
				return
			}
			if len(n.Children) != 2 || n.Split < 0.1 || n.Split > 0.9 {
				t.Fatal("invalid native split")
			}
			if n.Direction == "horizontal" {
				visit(n.Children[0], w*n.Split, h)
				visit(n.Children[1], w*(1-n.Split), h)
			} else {
				visit(n.Children[0], w, h*n.Split)
				visit(n.Children[1], w, h*(1-n.Split))
			}
		}
		visit(root, 1, 1)
		if count != shape.Columns*shape.Rows || focused != 1 {
			t.Fatalf("%v: %d cells, %d focused", shape, count, focused)
		}
	}
}
