package readiness

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Run directory files. run.json is the orchestrator's manifest; lanes.json
// and inventory.json are phase outputs; everything else is rendered from
// them or is a ledger the orchestrator appends to.
const (
	RunFile       = "run.json"
	LanesFile     = "lanes.json"
	InventoryFile = "inventory.json"
	ArgsFile      = "inventory-args.json"
)

// Run is run.json: one release-readiness run's frozen scope and authority.
type Run struct {
	V    int     `json:"v"`
	Date string  `json:"date"`
	Dir  string  `json:"dir"`
	Epic TaskRef `json:"epic"`
	// Rollup is the epic's QA task the final roll-up publishes to; created only
	// after every lane's story resolves to its own QA task.
	Rollup    TaskRef     `json:"rollup"`
	Authority Authority   `json:"authority"`
	Ranges    []RepoRange `json:"ranges"`
}

// TaskRef points at one vitrinka task.
type TaskRef struct {
	ID  TaskID `json:"id"`
	URL string `json:"url"`
}

// TaskID is a vitrinka task id. vitrinka answers numbers; hand-written run
// files often quote them, so both decode.
type TaskID string

// UnmarshalJSON accepts 123, "123" and null.
func (id *TaskID) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*id = ""
		return nil
	}
	if unq, err := strconv.Unquote(s); err == nil {
		s = unq
	}
	if s != "" {
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return fmt.Errorf("task id %s is not a number", string(b))
		}
	}
	*id = TaskID(s)
	return nil
}

// MarshalJSON writes the id as vitrinka does, a number (null when unset).
func (id TaskID) MarshalJSON() ([]byte, error) {
	if id == "" {
		return []byte("null"), nil
	}
	return []byte(id), nil
}

// Authority records the phase-0 answers; lanes act on them without asking.
type Authority struct {
	// Merge is how lane PRs merge, e.g. "/prm --auto --audit" or "human".
	Merge string `json:"merge"`
	// Devices are the matrix ids in scope for lane evidence.
	Devices []string `json:"devices"`
	// Concurrency is the most lane agents alive at once.
	Concurrency int `json:"concurrency"`
	// DeviceWalk adds a person-style device walk to the final phase.
	DeviceWalk bool `json:"deviceWalk"`
	// Finish is where the run stops (never the release itself).
	Finish string `json:"finish"`
}

// RepoRange is production..integration for one repo, frozen at init.
type RepoRange struct {
	Repo        string `json:"repo"`
	Path        string `json:"path"`
	GitHub      string `json:"github"`
	Production  string `json:"production"`
	ProdSHA     string `json:"productionSha"`
	Integration string `json:"integration"`
	IntSHA      string `json:"integrationSha"`
	Range       string `json:"range"`
	Commits     int    `json:"commits"`
}

// LaneSpec is one entry of lanes.json.
type LaneSpec struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Features and Journeys are "<cluster>.<index>" into inventory.json.
	Features []string `json:"features"`
	Journeys []string `json:"journeys,omitempty"`
	// Unassigned folds critic.unassigned[i] commits into this lane's body.
	Unassigned []int `json:"unassigned,omitempty"`
	// Stack is "full" (lane runs a dev stack) or "light" (tests only).
	Stack string  `json:"stack,omitempty"`
	Story TaskRef `json:"story,omitempty"`
	QA    TaskRef `json:"qa,omitempty"`
	Board string  `json:"board,omitempty"`
	Wave  string  `json:"wave,omitempty"`
}

// Inventory is inventory.json, the inventory workflow's output. Decoding is
// lenient: an older run's extra fields never block a render.
type Inventory struct {
	Inventories []Cluster `json:"inventories"`
	Critic      Critic    `json:"critic"`
}

// Cluster is one journey cluster's read of the range.
type Cluster struct {
	Lane            string    `json:"lane"`
	Features        []Feature `json:"features"`
	Journeys        []Journey `json:"journeys"`
	AttributedRefs  []string  `json:"attributedRefs"`
	ExcludedTooling []string  `json:"excludedTooling"`
	OpenQuestions   []string  `json:"openQuestions"`
}

// Feature is one user-facing change in the range.
type Feature struct {
	Name          string   `json:"name"`
	Summary       string   `json:"summary"`
	Refs          []string `json:"refs"`
	Surfaces      []string `json:"surfaces"`
	Roles         []string `json:"roles"`
	Devices       []string `json:"devices"`
	ExistingTests []string `json:"existingTests"`
	Gaps          []string `json:"gaps"`
	Risk          string   `json:"risk"`
	Repos         []string `json:"repos"`
}

// Journey is an end-to-end walk a cluster proposes.
type Journey struct {
	Name      string   `json:"name"`
	Steps     string   `json:"steps"`
	Roles     []string `json:"roles"`
	Devices   []string `json:"devices"`
	DataNeeds string   `json:"dataNeeds"`
}

// Critic is the completeness critic's attribution of the whole range.
type Critic struct {
	Notes        string       `json:"notes"`
	Overlaps     []Overlap    `json:"overlaps"`
	ToolingCount int          `json:"toolingCount"`
	Unassigned   []Unassigned `json:"unassigned"`
}

// Overlap is a ref claimed by several clusters and who owns it.
type Overlap struct {
	Ref   string   `json:"ref"`
	Lanes []string `json:"lanes"`
	Owner string   `json:"owner"`
}

// Unassigned is a functional commit no cluster claimed.
type Unassigned struct {
	Ref     string `json:"ref"`
	Repo    string `json:"repo"`
	Subject string `json:"subject"`
	Lane    string `json:"lane"`
}

// ReadRun loads run.json from dir.
func ReadRun(dir string) (Run, error) {
	var r Run
	data, err := os.ReadFile(filepath.Join(dir, RunFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, diag(DiagRunMissing, filepath.Join(dir, RunFile)+" does not exist", "readiness init --dir "+dir)
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, diag(DiagRunInvalid, RunFile+": "+err.Error(), "repair "+filepath.Join(dir, RunFile)+" (docs/readiness.md has its shape)")
	}
	return r, nil
}

// readOptional decodes dir/name into v; a missing file reports false.
func readOptional(dir, name string, v any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, diag(DiagRunInvalid, name+": "+err.Error(), "repair "+filepath.Join(dir, name)+" (docs/readiness.md has its shape)")
	}
	return true, nil
}

// refListing prints every valid "<cluster>.<index>" of inventory.json with its name.
const refListing = `jq -r '.inventories|to_entries[]|.key as $c|.value.features|to_entries[]|"\($c).\(.key) \(.value.name)"' inventory.json` +
	" (journeys: .value.journeys; both 0-based)"

// pick resolves "<cluster>.<index>" refs into items of the inventory.
func pick[T any](inv Inventory, refs []string, items func(Cluster) []T, kind, lane string) ([]T, error) {
	out := make([]T, 0, len(refs))
	for _, ref := range refs {
		c, i, ok := strings.Cut(ref, ".")
		ci, err1 := strconv.Atoi(c)
		ii, err2 := strconv.Atoi(i)
		if !ok || err1 != nil || err2 != nil || ci < 0 || ci >= len(inv.Inventories) {
			return nil, diag(DiagRunInvalid, fmt.Sprintf("lane %s: %s %q is not <cluster>.<index> of inventory.json", lane, kind, ref), refListing)
		}
		list := items(inv.Inventories[ci])
		if ii < 0 || ii >= len(list) {
			return nil, diag(DiagRunInvalid, fmt.Sprintf("lane %s: %s %q is out of range (cluster %d has %d)", lane, kind, ref, ci, len(list)), refListing)
		}
		out = append(out, list[ii])
	}
	return out, nil
}
