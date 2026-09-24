package catalog

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ItemKind string

const (
	KindApplet ItemKind = "applet"
	KindSkill  ItemKind = "skill"
	// KindTool is an external app or CLI Výbava installs through its own
	// published channel and detects live; it is never recorded in state.
	KindTool ItemKind = "tool"
)

type Status string

const (
	StatusStable       Status = "stable"
	StatusExperimental Status = "experimental"
)

type Catalog struct {
	SchemaVersion int     `yaml:"schema_version" json:"schema_version"`
	Items         []Item  `yaml:"items" json:"items"`
	Groups        []Group `yaml:"groups" json:"groups"`
}

type Item struct {
	ID          string   `yaml:"id" json:"id"`
	Kind        ItemKind `yaml:"kind" json:"kind"`
	Status      Status   `yaml:"status" json:"status"`
	Description string   `yaml:"description" json:"description"`
	Applet      string   `yaml:"applet,omitempty" json:"applet,omitempty"`
	Source      string   `yaml:"source,omitempty" json:"source,omitempty"`
	Tool        *Tool    `yaml:"tool,omitempty" json:"tool,omitempty"`
}

type Group struct {
	ID          string   `yaml:"id" json:"id"`
	Description string   `yaml:"description" json:"description"`
	Items       []string `yaml:"items" json:"items"`
}

func Load(source fs.FS) (Catalog, error) {
	data, err := fs.ReadFile(source, "catalog/catalog.yaml")
	if err != nil {
		return Catalog{}, fmt.Errorf("read catalog: %w", err)
	}

	var result Catalog
	if err := yaml.Unmarshal(data, &result); err != nil {
		return Catalog{}, fmt.Errorf("parse catalog: %w", err)
	}
	if err := result.Validate(source); err != nil {
		return Catalog{}, err
	}
	return result, nil
}

func (c Catalog) Validate(source fs.FS) error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("unsupported catalog schema_version %d", c.SchemaVersion)
	}

	items := make(map[string]Item, len(c.Items))
	for _, item := range c.Items {
		if !validID(item.ID) {
			return fmt.Errorf("item %q has an invalid id", item.ID)
		}
		if _, exists := items[item.ID]; exists {
			return fmt.Errorf("duplicate item id %q", item.ID)
		}
		if item.Description == "" {
			return fmt.Errorf("item %q has no description", item.ID)
		}
		if item.Status != StatusStable && item.Status != StatusExperimental {
			return fmt.Errorf("item %q has invalid status %q", item.ID, item.Status)
		}
		switch item.Kind {
		case KindApplet:
			if !validID(item.Applet) {
				return fmt.Errorf("applet item %q has invalid applet %q", item.ID, item.Applet)
			}
		case KindSkill:
			if item.Source != "skills/"+item.ID {
				return fmt.Errorf("skill item %q source must be skills/%s", item.ID, item.ID)
			}
			if _, err := fs.Stat(source, item.Source+"/SKILL.md"); err != nil {
				return fmt.Errorf("skill item %q source: %w", item.ID, err)
			}
		case KindTool:
			if err := item.Tool.validate(); err != nil {
				return fmt.Errorf("tool item %q: %w", item.ID, err)
			}
		default:
			return fmt.Errorf("item %q has invalid kind %q", item.ID, item.Kind)
		}
		items[item.ID] = item
	}
	for _, item := range c.Items {
		if item.Tool == nil {
			continue
		}
		for _, need := range item.Tool.Needs {
			if items[need].Kind != KindTool {
				return fmt.Errorf("tool item %q needs %q, which is not a tool item", item.ID, need)
			}
		}
	}

	groups := make(map[string]struct{}, len(c.Groups))
	for _, group := range c.Groups {
		if !validID(group.ID) {
			return fmt.Errorf("group %q has an invalid id", group.ID)
		}
		if _, exists := groups[group.ID]; exists {
			return fmt.Errorf("duplicate group id %q", group.ID)
		}
		groups[group.ID] = struct{}{}
		seen := make(map[string]struct{}, len(group.Items))
		for _, id := range group.Items {
			if _, exists := items[id]; !exists {
				return fmt.Errorf("group %q references unknown item %q", group.ID, id)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("group %q repeats item %q", group.ID, id)
			}
			seen[id] = struct{}{}
		}
	}
	return nil
}

func (c Catalog) Resolve(selectors []string) ([]Item, error) {
	if len(selectors) == 0 {
		selectors = []string{"recommended"}
	}

	items := make(map[string]Item, len(c.Items))
	for _, item := range c.Items {
		items[item.ID] = item
	}
	groups := make(map[string]Group, len(c.Groups))
	for _, group := range c.Groups {
		groups[group.ID] = group
	}

	selected := make(map[string]Item)
	for _, selector := range selectors {
		selector = strings.TrimPrefix(selector, "group:")
		if item, ok := items[selector]; ok {
			selected[item.ID] = item
			continue
		}
		if group, ok := groups[selector]; ok {
			for _, id := range group.Items {
				selected[id] = items[id]
			}
			continue
		}
		return nil, fmt.Errorf("unknown item or group %q", selector)
	}

	result := make([]Item, 0, len(selected))
	for _, item := range selected {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (c Catalog) GroupIDsFor(itemID string) []string {
	var result []string
	for _, group := range c.Groups {
		for _, id := range group.Items {
			if id == itemID {
				result = append(result, group.ID)
				break
			}
		}
	}
	sort.Strings(result)
	return result
}

func validID(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
