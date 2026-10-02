package transcripts

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ToolUse is one tool call an assistant message made.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is what one tool call returned into the model's context.
type ToolResult struct {
	UseID   string
	Text    string
	IsError bool
}

// contentBlock is one element of a message's content array; which fields are
// set depends on Type (text, tool_use, tool_result, image, thinking).
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// blocks decodes the content array; a plain-string content (a typed prompt)
// holds no blocks.
func (m ClaudeMessage) blocks() []contentBlock {
	var out []contentBlock
	if len(m.Content) == 0 || m.Content[0] != '[' || json.Unmarshal(m.Content, &out) != nil {
		return nil
	}
	return out
}

// Texts lists the text of a message, in order: a plain-string content is one
// text, an array contributes its text blocks.
func (m ClaudeMessage) Texts() []string {
	var s string
	if len(m.Content) > 0 && m.Content[0] == '"' {
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, b := range m.blocks() {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// ToolUses lists the tool calls of an assistant message, in order.
func (m ClaudeMessage) ToolUses() []ToolUse {
	var out []ToolUse
	for _, b := range m.blocks() {
		if b.Type == "tool_use" {
			out = append(out, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return out
}

// ToolResults lists the tool results a user record carries. Text joins the
// text blocks of an array result; images carry no text.
func (m ClaudeMessage) ToolResults() []ToolResult {
	var out []ToolResult
	for _, b := range m.blocks() {
		if b.Type != "tool_result" {
			continue
		}
		out = append(out, ToolResult{UseID: b.ToolUseID, Text: resultText(b.Content), IsError: b.IsError})
	}
	return out
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentBlock
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ClaudeToolLine is the cheap prefilter for tool traffic.
func ClaudeToolLine(line []byte) bool {
	return bytes.Contains(line, []byte(`"tool_use"`)) || bytes.Contains(line, []byte(`"tool_result"`))
}

// ClaudeToolRecord is a transcript line decoded for its tool traffic: the
// record plus toolUseResult, the harness's structured account of a result —
// what a Read returned, what an Edit changed.
type ClaudeToolRecord struct {
	ClaudeRecord
	// Subtype names a system record; "compact_boundary" marks a compaction,
	// after which the model no longer holds what it read before.
	Subtype       string          `json:"subtype"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// ClaudeCompactLine is the cheap prefilter for a compaction boundary.
func ClaudeCompactLine(line []byte) bool {
	return bytes.Contains(line, []byte(`"compact_boundary"`))
}

// DecodeClaudeTool decodes one transcript line with its toolUseResult.
func DecodeClaudeTool(line []byte) (ClaudeToolRecord, error) {
	var record ClaudeToolRecord
	err := json.Unmarshal(line, &record)
	return record, err
}

// ClaudeToolOutcome is the part of a toolUseResult readers use. Which fields
// are set depends on the tool.
type ClaudeToolOutcome struct {
	// File is what a Read returned: the range and the file's full length.
	File *struct {
		FilePath   string `json:"filePath"`
		StartLine  int    `json:"startLine"`
		NumLines   int    `json:"numLines"`
		TotalLines int    `json:"totalLines"`
	} `json:"file"`
	// FilePath, Type ("create" | "update"), Content and StructuredPatch
	// describe an Edit or Write; a created file has no patch, only content.
	FilePath        string `json:"filePath"`
	Type            string `json:"type"`
	Content         string `json:"content"`
	StructuredPatch []struct {
		Lines []string `json:"lines"`
	} `json:"structuredPatch"`
	// StaleHint marks a Bash command that changed a file the session had
	// read; StaleRecovered an Edit the harness had to re-read the file for.
	StaleHint      string `json:"staleReadFileStateHint"`
	StaleRecovered bool   `json:"staleRecovered"`
}

// Outcome decodes toolUseResult. ok is false when there is none or it is not
// an object (a failed call records its error as a string).
func (r ClaudeToolRecord) Outcome() (out ClaudeToolOutcome, ok bool) {
	if len(r.ToolUseResult) == 0 || r.ToolUseResult[0] != '{' {
		return out, false
	}
	err := json.Unmarshal(r.ToolUseResult, &out)
	return out, err == nil
}
