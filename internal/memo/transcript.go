package memo

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Citations is what one transcript said about the ledger: bare ids (`#45`,
// `^m45`, `[[LEDGER#^m45]]`), alias-scoped ids (`[[fixit-team/LEDGER#^m12]]`,
// `memo show fixit-team#12`), and every note file the Read tool opened.
type Citations struct {
	Cites     map[int]bool // bare #12, ^m12, [[LEDGER#^m12]]: the personal home
	Team      map[int]bool // #t12, ^t12, [[LEDGER#^t12]]: the team home
	Shows     map[int]bool
	TeamShows map[int]bool
	Scoped    map[string]map[int]bool // alias -> ids cited
	Reads     map[string]bool         // absolute paths of Read tool calls
}

func newCitations() Citations {
	return Citations{Cites: map[int]bool{}, Team: map[int]bool{}, Shows: map[int]bool{}, TeamShows: map[int]bool{}, Scoped: map[string]map[int]bool{}, Reads: map[string]bool{}}
}

var (
	hashCiteRE = regexp.MustCompile(`(?:^|[^\w#&/])#(t?)(\d+)\b`)
	blockRefRE = regexp.MustCompile(`\^([mt])(\d+)\b`)
	wikiCiteRE = regexp.MustCompile(`\[\[(?:([a-z0-9]+(?:-[a-z0-9]+)*)/)?LEDGER#\^([mt])(\d+)\]\]`)
	showCmdRE  = regexp.MustCompile(`\bmemo show ((?:[a-z0-9]+(?:-[a-z0-9]+)*)?#?t?\d+)\b`)
)

type transcriptRow struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type transcriptBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

// ScanTranscript reads a Claude Code transcript (jsonl) and collects the
// citations in assistant text and tool inputs. Malformed lines are skipped;
// only the file itself being unreadable is an error.
func ScanTranscript(path string) (Citations, error) {
	f, err := os.Open(path)
	if err != nil {
		return Citations{}, err
	}
	defer f.Close()
	return scanTranscript(f)
}

func scanTranscript(r io.Reader) (Citations, error) {
	c := newCitations()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var row transcriptRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil || row.Type != "assistant" {
			continue
		}
		for _, b := range contentBlocks(row.Message.Content) {
			switch b.Type {
			case "text":
				c.collect(b.Text)
			case "tool_use":
				c.collectToolUse(b.Name, b.Input)
			}
		}
	}
	return c, sc.Err()
}

func contentBlocks(raw json.RawMessage) []transcriptBlock {
	if len(raw) == 0 || raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []transcriptBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []transcriptBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

func (c *Citations) collect(text string) {
	for _, m := range wikiCiteRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[3])
		if m[1] != "" {
			c.scoped(m[1])[id] = true
		} else {
			c.cite(m[2] == "t", id)
		}
	}
	for _, m := range hashCiteRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[2])
		c.cite(m[1] == "t", id)
	}
	for _, m := range blockRefRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[2])
		c.cite(m[1] == "t", id)
	}
}

func (c *Citations) scoped(alias string) map[int]bool {
	if c.Scoped[alias] == nil {
		c.Scoped[alias] = map[int]bool{}
	}
	return c.Scoped[alias]
}

func (c *Citations) collectToolUse(name string, input json.RawMessage) {
	var fields struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	}
	_ = json.Unmarshal(input, &fields)
	switch name {
	case "Read":
		if strings.HasSuffix(fields.FilePath, ".md") {
			c.Reads[fields.FilePath] = true
		}
	case "Bash":
		for _, m := range showCmdRE.FindAllStringSubmatch(fields.Command, -1) {
			if ref, d := ParseRef(m[1]); d == nil {
				switch {
				case ref.Alias != "":
					c.scoped(ref.Alias)[ref.ID] = true
				case ref.Team:
					c.TeamShows[ref.ID] = true
				default:
					c.Shows[ref.ID] = true
				}
			}
		}
	}
	c.collect(string(input))
}

func (c *Citations) cite(team bool, id int) {
	if team {
		c.Team[id] = true
	} else {
		c.Cites[id] = true
	}
}
