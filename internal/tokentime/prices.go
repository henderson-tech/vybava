package tokentime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Price is a model's list API price in USD per million tokens. OpenAI has one
// cache-write rate; it fills both TTL columns.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
	CacheRead    float64 `json:"cacheRead"`
}

// PricesAsOf dates the built-in table. Sources: Anthropic's pricing page
// (https://platform.claude.com/docs/en/about-claude/pricing) as cached by the
// claude-api skill on 2026-06-24 plus the Opus 5.5 / Fable 5.1 cache-read
// rates it documents, and OpenAI's pricing page
// (https://developers.openai.com/api/docs/pricing) read 2026-09-23. Standard
// tier, short-context rates; OpenAI's long-context surcharge is not applied.
const PricesAsOf = "2026-09-23"

// claude builds an Anthropic row from its input/output rates and the standard
// cache multipliers: 1.25× 5-minute writes, 2× 1-hour writes, 0.1× reads.
func claude(input, output float64) Price {
	return Price{Input: input, Output: output, CacheWrite5m: input * 1.25, CacheWrite1h: input * 2, CacheRead: input / 10}
}

func openai(input, cached, write, output float64) Price {
	return Price{Input: input, Output: output, CacheWrite5m: write, CacheWrite1h: write, CacheRead: cached}
}

// builtinPrices is the table the rollup prices with unless the state
// directory's prices.json overrides a row. A model missing here is reported
// as unpriced — never guessed.
var builtinPrices = map[string]Price{
	"claude-fable-5-1":  withRead(claude(10, 50), 0.25),
	"claude-fable-5":    claude(10, 50),
	"claude-opus-5-5":   withRead(claude(4, 20), 0.20),
	"claude-opus-5":     claude(5, 25),
	"claude-opus-4-8":   claude(5, 25),
	"claude-opus-4-7":   claude(5, 25),
	"claude-opus-4-6":   claude(5, 25),
	"claude-sonnet-5":   claude(2, 10),
	"claude-sonnet-4-6": claude(3, 15),
	"claude-haiku-4-5":  claude(1, 5),

	"gpt-6-astra":   openai(10, 1, 12.5, 50),
	"gpt-5.6-sol":   openai(4, 0.4, 5, 20), // promotional through 2026-11-21
	"gpt-5.6-terra": openai(2, 0.2, 2.5, 12),
	"gpt-5.6-luna":  openai(0.2, 0.02, 0.25, 1.2),
	"gpt-5.6-cyber": openai(12.5, 1.25, 15.625, 75),
	"gpt-5.5":       openai(5, 0.5, 5, 30), // no cache-write rate: writes bill as input
}

// aliases are names that bill as another model. OpenAI's Daybreak names
// point at the model currently behind them.
var aliases = map[string]string{
	"gpt-daybreak-blue": "gpt-5.6-sol",
	"gpt-daybreak-red":  "gpt-5.6-cyber",
}

func withRead(p Price, read float64) Price {
	p.CacheRead = read
	return p
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// CanonicalModel strips what does not change the price: a context-window tag
// ("[1m]"), a dated snapshot suffix and a "-latest" alias suffix.
func CanonicalModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i > 0 {
		m = m[:i]
	}
	m = strings.TrimSuffix(m, "-latest")
	m = dateSuffix.ReplaceAllString(m, "")
	if target, ok := aliases[m]; ok {
		return target
	}
	return m
}

// Prices is the effective table: built-ins merged with an override file.
type Prices struct {
	Table        map[string]Price
	OverridePath string
	Overridden   []string
	// Incomplete names override rows for models the built-in table does not
	// know that leave a rate out: those components price at $0, so it is said.
	Incomplete []string
}

// priceOverride is one prices.json row; an absent field keeps the built-in rate.
type priceOverride struct {
	Input        *float64 `json:"input"`
	Output       *float64 `json:"output"`
	CacheWrite5m *float64 `json:"cacheWrite5m"`
	CacheWrite1h *float64 `json:"cacheWrite1h"`
	CacheRead    *float64 `json:"cacheRead"`
}

// LoadPrices merges <stateDir>/prices.json over the built-in table. The file
// maps model id → Price; a row merges field by field over the built-in row,
// so overriding one rate never zeroes the others. A missing file is not an error.
func LoadPrices(stateDir string) (Prices, error) {
	p := Prices{Table: map[string]Price{}, OverridePath: filepath.Join(stateDir, "prices.json")}
	for k, v := range builtinPrices {
		p.Table[k] = v
	}
	raw, err := os.ReadFile(p.OverridePath)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var override map[string]priceOverride
	if err := json.Unmarshal(raw, &override); err != nil {
		return p, fmt.Errorf("%s: %w", p.OverridePath, err)
	}
	for k, o := range override {
		k = CanonicalModel(k)
		row, builtin := p.Table[k]
		var missing []string
		for _, f := range []struct {
			name string
			src  *float64
			dst  *float64
		}{
			{"input", o.Input, &row.Input}, {"output", o.Output, &row.Output},
			{"cacheWrite5m", o.CacheWrite5m, &row.CacheWrite5m}, {"cacheWrite1h", o.CacheWrite1h, &row.CacheWrite1h},
			{"cacheRead", o.CacheRead, &row.CacheRead},
		} {
			if f.src != nil {
				*f.dst = *f.src
			} else if !builtin {
				missing = append(missing, f.name)
			}
		}
		p.Table[k] = row
		p.Overridden = append(p.Overridden, k)
		if len(missing) > 0 {
			p.Incomplete = append(p.Incomplete, k+" (no "+strings.Join(missing, ", ")+")")
		}
	}
	sort.Strings(p.Overridden)
	sort.Strings(p.Incomplete)
	return p, nil
}

// Lookup returns the price row for a model, if any.
func (p Prices) Lookup(model string) (Price, bool) {
	price, ok := p.Table[CanonicalModel(model)]
	return price, ok
}

// Cost prices one set of counts; ok is false for an unpriced model.
func (p Prices) Cost(model string, c Counts) (float64, bool) {
	price, ok := p.Lookup(model)
	if !ok {
		return 0, false
	}
	const perToken = 1.0 / 1_000_000
	return (float64(c.Input)*price.Input + float64(c.Output)*price.Output +
		float64(c.CacheWrite5m)*price.CacheWrite5m + float64(c.CacheWrite1h)*price.CacheWrite1h +
		float64(c.CacheRead)*price.CacheRead) * perToken, true
}
