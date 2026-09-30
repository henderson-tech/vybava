package polishkit

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Sheet geometry. Pure Go: image, image/draw, image/png only.
const (
	// SheetCellHeight is the common height every shot is scaled to.
	SheetCellHeight = 640
	// SheetLabel is the label strip above each cell.
	SheetLabel = 28
	// SheetGap separates cells.
	SheetGap = 12
	// EdgeCrop is the corner crop size in source px; EdgeZoom its zoom.
	EdgeCrop = 120
	EdgeZoom = 3
	// EdgeBand is the height of the top and bottom bands in source px.
	EdgeBand = 120
	// EdgeWidth is the edges sheet's width: four zoomed corners and gaps.
	EdgeWidth  = 4*EdgeCrop*EdgeZoom + 3*SheetGap
	labelScale = 2
)

var (
	sheetBG    = color.RGBA{0x22, 0x22, 0x26, 0xff}
	labelBG    = color.RGBA{0x3a, 0x3a, 0x40, 0xff}
	labelFG    = color.RGBA{0xf2, 0xf2, 0xf2, 0xff}
	missingBG  = color.RGBA{0x55, 0x2a, 0x2a, 0xff}
	edgeMarker = color.RGBA{0xff, 0x5c, 0x5c, 0xff}
)

// SheetOptions are the sheet verb's flags.
type SheetOptions struct {
	Pass   int
	Screen string
	Lanes  []string
}

// SheetFile is one screen's pair of sheets.
type SheetFile struct {
	Screen string `json:"screen"`
	File   string `json:"file"`
	Edges  string `json:"edges"`
	Legend string `json:"legend"`
	Shots  int    `json:"shots"`
	// Missing lists the cells of the grid without a readable shot.
	Missing []string `json:"missing"`
}

// SheetData is what sheet reports.
type SheetData struct {
	Pass   int         `json:"pass"`
	Dir    string      `json:"dir"`
	Sheets []SheetFile `json:"sheets"`
}

// SheetState is one row of the grid: a device state.
type SheetState struct {
	Theme    string `json:"theme"`
	Nav      string `json:"nav,omitempty"`
	TextSize string `json:"textSize,omitempty"`
}

// Label is the row's short name.
func (s SheetState) Label() string {
	parts := []string{s.Theme}
	if s.Nav != "" {
		parts = append(parts, s.Nav)
	}
	if s.TextSize != "" {
		parts = append(parts, s.TextSize)
	}
	return strings.Join(parts, " ")
}

// Legend is the sheet's sidecar: where each cell sits.
type Legend struct {
	Screen  string       `json:"screen"`
	Columns []string     `json:"columns"`
	Rows    []SheetState `json:"rows"`
	Cells   []LegendCell `json:"cells"`
}

// LegendCell is one placed shot.
type LegendCell struct {
	Cell   string `json:"cell"`
	Column int    `json:"column"`
	Row    int    `json:"row"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Shot   string `json:"shot"`
	Loaded bool   `json:"loaded"`
}

// Grid is the sheet layout for one screen: columns are lanes, rows are
// device states, each cell scaled to SheetCellHeight.
type Grid struct {
	Screen  string
	Columns []string
	Rows    []SheetState
	// Cells: [row][col] the cell placed there (nil when none).
	Cells [][]*Cell
	// Widths: per column, the widest scaled shot.
	Widths []int
}

// BuildGrid lays a screen's chrome cells out; sizes tells a shot's
// dimensions (0,0 when unreadable).
func BuildGrid(run *RunFile, screen string, lanes []string, sizes func(c Cell) (w, h int)) Grid {
	g := Grid{Screen: screen}
	for _, l := range run.Lanes {
		if len(lanes) > 0 && !slices.Contains(lanes, l.ID) {
			continue
		}
		g.Columns = append(g.Columns, l.ID)
	}
	col := func(lane string) int { return slices.Index(g.Columns, lane) }
	rowOf := map[SheetState]int{}
	for i := range run.Cells {
		c := &run.Cells[i]
		if c.Kind != CellChrome || c.Screen != screen || col(c.Lane) < 0 || c.Shot == "" {
			continue
		}
		st := SheetState{Theme: c.Theme, Nav: c.Nav, TextSize: c.TextSize}
		if _, ok := rowOf[st]; !ok {
			rowOf[st] = len(g.Rows)
			g.Rows = append(g.Rows, st)
			g.Cells = append(g.Cells, make([]*Cell, len(g.Columns)))
		}
		g.Cells[rowOf[st]][col(c.Lane)] = c
	}
	g.Widths = make([]int, len(g.Columns))
	for r := range g.Rows {
		for ci, c := range g.Cells[r] {
			if c == nil {
				continue
			}
			w, h := sizes(*c)
			sw := scaledWidth(w, h, SheetCellHeight)
			if sw > g.Widths[ci] {
				g.Widths[ci] = sw
			}
		}
	}
	for ci := range g.Widths {
		if g.Widths[ci] == 0 {
			g.Widths[ci] = SheetCellHeight * 9 / 19 // a phone's footprint for an empty column
		}
	}
	return g
}

// scaledWidth keeps the aspect when scaling to height h.
func scaledWidth(w, h, toH int) int {
	if w <= 0 || h <= 0 {
		return 0
	}
	return max(1, w*toH/h)
}

// Size is the sheet's pixel size.
func (g Grid) Size() (w, h int) {
	for _, cw := range g.Widths {
		w += cw
	}
	w += SheetGap * (len(g.Widths) + 1)
	h = SheetGap + len(g.Rows)*(SheetLabel+SheetCellHeight+SheetGap)
	return w, h
}

// Origin is the top-left of a cell's label strip.
func (g Grid) Origin(row, col int) (x, y int) {
	x = SheetGap
	for ci := 0; ci < col; ci++ {
		x += g.Widths[ci] + SheetGap
	}
	y = SheetGap + row*(SheetLabel+SheetCellHeight+SheetGap)
	return x, y
}

// Sheet renders one contact sheet and one edges sheet per screen of the pass.
func (t *Tool) Sheet(opts SheetOptions) (Result, error) {
	run, err := t.LoadRun(opts.Pass)
	if err != nil {
		return Result{}, err
	}
	for _, id := range opts.Lanes {
		if _, ok := runLane(run, id); !ok {
			return Result{}, diag(DiagUnknownLane, fmt.Sprintf("pass %d does not include lane %q (lanes: %s)", run.Pass, id, strings.Join(runLaneIDs(run), ", ")), fmt.Sprintf("polish-kit status --pass %d --json", run.Pass))
		}
	}
	var screens []Screen
	if opts.Screen != "" {
		s, ok := screenOf(run, opts.Screen)
		if !ok {
			return Result{}, diag(DiagUnknownScreen, fmt.Sprintf("pass %d has no screen %q", run.Pass, opts.Screen), fmt.Sprintf("polish-kit status --pass %d --json", run.Pass))
		}
		screens = []Screen{s}
	} else {
		screens = run.Screens
	}
	dir := filepath.Join(run.PassDir, "sheets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	data := SheetData{Pass: run.Pass, Dir: dir, Sheets: []SheetFile{}}
	for _, s := range screens {
		images := map[string]image.Image{}
		load := func(c Cell) image.Image {
			if img, ok := images[c.ID]; ok {
				return img
			}
			img, err := readPNG(run.ShotPath(c))
			if err != nil {
				fmt.Fprintf(t.Log, "sheet: %s: %v\n", c.Shot, err)
				img = nil
			}
			images[c.ID] = img
			return img
		}
		grid := BuildGrid(run, s.ID, opts.Lanes, func(c Cell) (int, int) {
			if img := load(c); img != nil {
				b := img.Bounds()
				return b.Dx(), b.Dy()
			}
			return 0, 0
		})
		if len(grid.Rows) == 0 {
			continue
		}
		sf := SheetFile{Screen: s.ID, Missing: []string{}}
		sf.File = filepath.Join(dir, s.ID+".png")
		sf.Edges = filepath.Join(dir, s.ID+"--edges.png")
		sf.Legend = filepath.Join(dir, s.ID+".json")
		legend, shots, missing := renderSheet(grid, load, sf.File)
		sf.Shots, sf.Missing = shots, missing
		if err := writePNG(sf.File, legend.img); err != nil {
			return Result{Data: data}, err
		}
		if err := writeJSON(sf.Legend, legend.Legend); err != nil {
			return Result{Data: data}, err
		}
		if err := writePNG(sf.Edges, renderEdges(grid, load)); err != nil {
			return Result{Data: data}, err
		}
		data.Sheets = append(data.Sheets, sf)
	}
	if len(data.Sheets) == 0 {
		return Result{Data: data}, diag(DiagShotRequired, fmt.Sprintf("pass %d holds no shot to lay out", run.Pass), fmt.Sprintf("polish-kit shoot %s --pass %d --json", firstLane(run), run.Pass))
	}
	lines := []string{}
	for _, sf := range data.Sheets {
		lines = append(lines, fmt.Sprintf("%s: %d shots -> %s (+ edges, legend)", sf.Screen, sf.Shots, sf.File))
	}
	next := []string{}
	if pending := pendingCells(run); len(pending) > 0 {
		next = append(next, cellCommand(pending[0], run.Pass))
	}
	next = append(next, fmt.Sprintf("polish-kit report --pass %d --json", run.Pass))
	return Result{Data: data, Lines: lines, Next: next}, nil
}

type renderedLegend struct {
	Legend
	img *image.RGBA
}

// renderSheet paints the grid: label strip, then the shot scaled to the
// common height (a missing shot is a red cell).
func renderSheet(g Grid, load func(Cell) image.Image, file string) (renderedLegend, int, []string) {
	w, h := g.Size()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(sheetBG), image.Point{}, draw.Src)
	legend := renderedLegend{Legend: Legend{Screen: g.Screen, Columns: g.Columns, Rows: g.Rows, Cells: []LegendCell{}}, img: img}
	shots := 0
	missing := []string{}
	for r, row := range g.Rows {
		for ci, c := range g.Cells[r] {
			if c == nil {
				continue
			}
			x, y := g.Origin(r, ci)
			cw := g.Widths[ci]
			draw.Draw(img, image.Rect(x, y, x+cw, y+SheetLabel), image.NewUniform(labelBG), image.Point{}, draw.Src)
			drawText(img, x+6, y+(SheetLabel-glyphH*labelScale)/2, fitLabel(c.Lane+" "+row.Label(), cw-12, labelScale), labelScale, labelFG)
			lc := LegendCell{Cell: c.ID, Column: ci, Row: r, X: x, Y: y + SheetLabel, W: cw, H: SheetCellHeight, Shot: c.Shot}
			src := load(*c)
			if src == nil {
				draw.Draw(img, image.Rect(x, y+SheetLabel, x+cw, y+SheetLabel+SheetCellHeight), image.NewUniform(missingBG), image.Point{}, draw.Src)
				drawText(img, x+6, y+SheetLabel+8, "MISSING SHOT", labelScale, labelFG)
				missing = append(missing, c.ID)
			} else {
				b := src.Bounds()
				sw := scaledWidth(b.Dx(), b.Dy(), SheetCellHeight)
				scaled := scaleBox(src, sw, SheetCellHeight)
				draw.Draw(img, image.Rect(x, y+SheetLabel, x+sw, y+SheetLabel+SheetCellHeight), scaled, image.Point{}, draw.Src)
				lc.W, lc.Loaded = sw, true
				shots++
			}
			legend.Cells = append(legend.Cells, lc)
		}
	}
	return legend, shots, missing
}

// fitLabel truncates a label to the pixel width.
func fitLabel(s string, width, scale int) string {
	r := []rune(s)
	for len(r) > 0 && textWidth(string(r), scale) > width {
		r = r[:len(r)-1]
	}
	return string(r)
}

// EdgeBlockHeight is one shot's block on the edges sheet: a label, the
// four corners at EdgeZoom, then the top and bottom bands.
func edgeBlockHeight(srcW int) int {
	return SheetLabel + EdgeCrop*EdgeZoom + SheetGap + 2*(bandHeight(srcW)+SheetGap)
}

// bandZoom fits a band to the sheet width, never above EdgeZoom.
func bandZoom(srcW int) float64 {
	if srcW <= 0 {
		return 1
	}
	z := float64(EdgeWidth) / float64(srcW)
	if z > EdgeZoom {
		return EdgeZoom
	}
	return z
}

func bandHeight(srcW int) int { return int(float64(EdgeBand) * bandZoom(srcW)) }

// renderEdges paints, per shot, the four corners zoomed EdgeZoom times and
// the top and bottom EdgeBand px bands fitted to the width: clip and rim
// defects hide at the edges.
func renderEdges(g Grid, load func(Cell) image.Image) *image.RGBA {
	type item struct {
		cell *Cell
		row  SheetState
		img  image.Image
	}
	var items []item
	for r, row := range g.Rows {
		for _, c := range g.Cells[r] {
			if c == nil {
				continue
			}
			if img := load(*c); img != nil {
				items = append(items, item{c, row, img})
			}
		}
	}
	h := SheetGap
	for _, it := range items {
		h += edgeBlockHeight(it.img.Bounds().Dx()) + SheetGap
	}
	if h == SheetGap {
		h += SheetLabel + SheetGap
	}
	out := image.NewRGBA(image.Rect(0, 0, EdgeWidth+2*SheetGap, h))
	draw.Draw(out, out.Bounds(), image.NewUniform(sheetBG), image.Point{}, draw.Src)
	y := SheetGap
	for _, it := range items {
		b := it.img.Bounds()
		x := SheetGap
		draw.Draw(out, image.Rect(x, y, x+EdgeWidth, y+SheetLabel), image.NewUniform(labelBG), image.Point{}, draw.Src)
		drawText(out, x+6, y+(SheetLabel-glyphH*labelScale)/2, fitLabel(it.cell.Lane+" "+it.row.Label()+" CORNERS 3X, BANDS", EdgeWidth-12, labelScale), labelScale, labelFG)
		y += SheetLabel
		cw, ch := min(EdgeCrop, b.Dx()), min(EdgeCrop, b.Dy())
		corners := []image.Rectangle{
			image.Rect(b.Min.X, b.Min.Y, b.Min.X+cw, b.Min.Y+ch),
			image.Rect(b.Max.X-cw, b.Min.Y, b.Max.X, b.Min.Y+ch),
			image.Rect(b.Min.X, b.Max.Y-ch, b.Min.X+cw, b.Max.Y),
			image.Rect(b.Max.X-cw, b.Max.Y-ch, b.Max.X, b.Max.Y),
		}
		for i, rect := range corners {
			cx := x + i*(EdgeCrop*EdgeZoom+SheetGap)
			zoomed := scaleNearest(crop(it.img, rect), cw*EdgeZoom, ch*EdgeZoom)
			draw.Draw(out, image.Rect(cx, y, cx+cw*EdgeZoom, y+ch*EdgeZoom), zoomed, image.Point{}, draw.Src)
			// a 1 px marker on the sheet's outer edge of each corner shows where the device edge is
			markCorner(out, cx, y, cw*EdgeZoom, ch*EdgeZoom, i)
		}
		y += EdgeCrop*EdgeZoom + SheetGap
		bh := bandHeight(b.Dx())
		bandH := min(EdgeBand, b.Dy())
		for _, rect := range []image.Rectangle{
			image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+bandH),
			image.Rect(b.Min.X, b.Max.Y-bandH, b.Max.X, b.Max.Y),
		} {
			zoomed := scaleNearest(crop(it.img, rect), EdgeWidth, bh)
			draw.Draw(out, image.Rect(x, y, x+EdgeWidth, y+bh), zoomed, image.Point{}, draw.Src)
			y += bh + SheetGap
		}
	}
	return out
}

// markCorner draws the two device edges of corner i (0 tl, 1 tr, 2 bl, 3 br).
func markCorner(dst *image.RGBA, x, y, w, h, i int) {
	m := image.NewUniform(edgeMarker)
	top, left := i < 2, i%2 == 0
	if top {
		draw.Draw(dst, image.Rect(x, y, x+w, y+1), m, image.Point{}, draw.Src)
	} else {
		draw.Draw(dst, image.Rect(x, y+h-1, x+w, y+h), m, image.Point{}, draw.Src)
	}
	if left {
		draw.Draw(dst, image.Rect(x, y, x+1, y+h), m, image.Point{}, draw.Src)
	} else {
		draw.Draw(dst, image.Rect(x+w-1, y, x+w, y+h), m, image.Point{}, draw.Src)
	}
}

// crop copies a sub-rectangle into a fresh RGBA.
func crop(src image.Image, r image.Rectangle) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), src, r.Min, draw.Src)
	return out
}

// scaleNearest zooms by nearest neighbour: crisp pixels, what an edge
// inspection wants.
func scaleNearest(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if b.Dx() == 0 || b.Dy() == 0 || w == 0 || h == 0 {
		return out
	}
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			out.Set(x, y, src.At(sx, sy))
		}
	}
	return out
}

// scaleBox shrinks by averaging each destination pixel's source box (and
// falls back to nearest when enlarging).
func scaleBox(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	if w >= b.Dx() || h >= b.Dy() {
		return scaleNearest(src, w, h)
	}
	rgba, ok := src.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(b)
		draw.Draw(rgba, b, src, b.Min, draw.Src)
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+(y+1)*b.Dy()/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+(x+1)*b.Dx()/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				i := rgba.PixOffset(x0, sy)
				for sx := x0; sx < x1; sx++ {
					r += uint64(rgba.Pix[i])
					g += uint64(rgba.Pix[i+1])
					bl += uint64(rgba.Pix[i+2])
					a += uint64(rgba.Pix[i+3])
					n++
					i += 4
				}
			}
			o := out.PixOffset(x, y)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), uint8(a/n)
		}
	}
	return out
}

func readPNG(path string) (image.Image, error) {
	if path == "" {
		return nil, fmt.Errorf("no shot")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
