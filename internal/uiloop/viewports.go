package uiloop

import "sort"

// BuiltinViewports mirrors BUILTIN_VIEWPORTS in harness/viewports.ts; a test
// keeps the two tables equal. Every shot is DPR 2.
var BuiltinViewports = map[string]Viewport{
	"phone":           {Width: 390, Height: 844, Mobile: true, Insets: &Insets{Top: 59, Bottom: 34}},
	"phone-320":       {Width: 320, Height: 568, Mobile: true, Insets: &Insets{Top: 20}},
	"phone-360":       {Width: 360, Height: 780, Mobile: true},
	"phone-landscape": {Width: 844, Height: 390, Mobile: true, Insets: &Insets{Right: 47, Bottom: 21, Left: 47}},
	"tablet":          {Width: 820, Height: 1180, Mobile: true},
	"laptop":          {Width: 1024, Height: 768},
	"desktop":         {Width: 1440, Height: 810},
}

// DPR is the device scale factor of every shot.
const DPR = 2

// BuiltinViewportIDs in sorted order.
func BuiltinViewportIDs() []string {
	ids := make([]string, 0, len(BuiltinViewports))
	for id := range BuiltinViewports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
