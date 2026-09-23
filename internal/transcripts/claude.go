package transcripts

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeRecord is one line of a Claude Code transcript. Only the fields any
// reader needs are decoded; message content stays raw and is never copied by
// usage readers.
type ClaudeRecord struct {
	Type        string        `json:"type"`
	UUID        string        `json:"uuid"`
	SessionID   string        `json:"sessionId"`
	Cwd         string        `json:"cwd"`
	Timestamp   time.Time     `json:"timestamp"`
	IsSidechain bool          `json:"isSidechain"`
	AgentID     string        `json:"agentId"`
	RequestID   string        `json:"requestId"`
	Message     ClaudeMessage `json:"message"`
}

// ClaudeMessage is the API message an assistant record carries.
type ClaudeMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   *ClaudeUsage    `json:"usage"`
	Content json.RawMessage `json:"content"`
}

// ClaudeUsage is the Messages API usage block. Anthropic's components are
// already disjoint: input excludes both cache reads and cache writes, and
// output includes thinking.
type ClaudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// CacheWrites splits cache creation by TTL. Writes the split does not account
// for (older transcripts carry no split) count as 5-minute writes.
func (u ClaudeUsage) CacheWrites() (fiveMinute, oneHour int64) {
	if u.CacheCreation != nil {
		oneHour = min(u.CacheCreation.Ephemeral1h, u.CacheCreationInputTokens)
	}
	return u.CacheCreationInputTokens - oneHour, oneHour
}

// SyntheticModel marks locally generated assistant records (errors, interrupts)
// that never reached the API; their usage is always zero.
const SyntheticModel = "<synthetic>"

// ClaudeUsageLine is the cheap prefilter a usage reader runs before decoding:
// most transcript bytes are tool output in user records.
func ClaudeUsageLine(line []byte) bool {
	return bytes.Contains(line, []byte(`"assistant"`)) && bytes.Contains(line, []byte(`"usage"`))
}

// DecodeClaude decodes one transcript line.
func DecodeClaude(line []byte) (ClaudeRecord, error) {
	var record ClaudeRecord
	err := json.Unmarshal(line, &record)
	return record, err
}

// ClaudeFileKind says where a transcript sits in the projects tree.
type ClaudeFileKind string

const (
	// ClaudeSession is <slug>/<session>.jsonl.
	ClaudeSession ClaudeFileKind = "session"
	// ClaudeSubagent is <slug>/<session>/subagents/agent-<id>.jsonl.
	ClaudeSubagent ClaudeFileKind = "subagent"
	// ClaudeWorkflowAgent is <slug>/<session>/subagents/workflows/<run>/agent-<id>.jsonl.
	ClaudeWorkflowAgent ClaudeFileKind = "workflow-agent"
)

// ClaudeFile is one transcript found under a projects root.
type ClaudeFile struct {
	Path string
	Kind ClaudeFileKind
	Info os.FileInfo
}

// WalkClaude lists every transcript under root (~/.claude/projects): main
// sessions, subagents and workflow agents. Workflow journals, memory usage
// logs and *.meta.json sidecars are not transcripts and are excluded. A
// missing root is empty, not an error. skipped counts what the walk could not
// see (a missing root, an unreadable directory, an entry that vanished
// mid-walk): while it is > 0 a caller must not treat an unlisted file as gone.
func WalkClaude(root string) (files []ClaudeFile, skipped int, err error) {
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				skipped++
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if entry.IsDir() {
			if len(parts) >= 6 {
				return filepath.SkipDir
			}
			return nil
		}
		kind, ok := claudeKind(parts)
		if !ok || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			skipped++ // removed after the directory listing
			return nil
		}
		files = append(files, ClaudeFile{Path: path, Kind: kind, Info: info})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, skipped, err
	}
	return files, skipped, nil
}

func claudeKind(parts []string) (ClaudeFileKind, bool) {
	name := parts[len(parts)-1]
	if !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	agent := strings.HasPrefix(name, "agent-")
	switch {
	case len(parts) == 2:
		return ClaudeSession, true
	case len(parts) == 4 && parts[2] == "subagents" && agent:
		return ClaudeSubagent, true
	case len(parts) == 6 && parts[2] == "subagents" && parts[3] == "workflows" && agent:
		return ClaudeWorkflowAgent, true
	}
	return "", false
}
