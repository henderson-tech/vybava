package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Row is one checklist line: what it is, whether it is already on the
// machine, and whether it starts ticked.
type Row struct {
	ID          string
	Kind        string
	Description string
	Installed   bool
	Optional    bool
	Checked     bool
}

type checklistModel struct {
	title     string
	rows      []Row
	cursor    int
	confirmed bool
}

// Checklist lets a human tick rows and returns the checked IDs; confirmed is
// false when they cancelled. It is a view only — every flow it drives is
// also reachable non-interactively (--yes / --only / --with).
func Checklist(title string, rows []Row) ([]string, bool, error) {
	final, err := tea.NewProgram(checklistModel{title: title, rows: rows}).Run()
	if err != nil {
		return nil, false, err
	}
	result, ok := final.(checklistModel)
	if !ok || !result.confirmed {
		return nil, false, nil
	}
	var ids []string
	for _, row := range result.rows {
		if row.Checked {
			ids = append(ids, row.ID)
		}
	}
	return ids, true, nil
}

func (m checklistModel) Init() tea.Cmd { return nil }

func (m checklistModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "space":
			m.rows[m.cursor].Checked = !m.rows[m.cursor].Checked
		case "a":
			for i := range m.rows {
				m.rows[i].Checked = !m.rows[i].Optional || m.rows[i].Checked
			}
		case "enter":
			m.confirmed = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m checklistModel) View() tea.View {
	var view strings.Builder
	view.WriteString(m.title + "\n\n")
	for i, row := range m.rows {
		cursor := " "
		if i == m.cursor {
			cursor = ">"
		}
		checked := " "
		if row.Checked {
			checked = "x"
		}
		state := "missing"
		if row.Installed {
			state = "installed"
		}
		if row.Optional {
			state += " · optional"
		}
		fmt.Fprintf(&view, "%s [%s] %-16s %-7s %s\n", cursor, checked, row.ID, row.Kind, state)
		if i == m.cursor {
			fmt.Fprintf(&view, "      %s\n", row.Description)
		}
	}
	view.WriteString("\n↑/↓ move  space toggle  a all required  enter apply  q cancel\n")
	return tea.NewView(view.String())
}
