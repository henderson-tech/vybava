// Package assets exposes Výbava's versioned catalog and installable payloads.
package assets

import "embed"

// FS is the immutable payload shipped inside the Výbava binary.
//
// The bare "skills" pattern walks the whole payload tree — a skill is not
// always a lone SKILL.md; press-pdf ships references/ and an assets/ pipeline.
// Without the all: prefix Go skips _ and . prefixed entries, which keeps
// __pycache__ and editor droppings out of the binary for free.
//
// Mods need the all: prefix: the plugin manifest lives in .claude-plugin/.
// The installer skips what that drags in — the engine-written
// .claude-plugin/types/ a local load lays beside a mod, and .DS_Store.
//
//go:embed catalog/catalog.yaml skills all:mods
var FS embed.FS
