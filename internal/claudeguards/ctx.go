package claudeguards

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ToolCost struct {
	Tool            string `json:"tool"`
	Calls           int    `json:"calls"`
	EstimatedTokens int    `json:"estimatedTokens"`
	Images          int    `json:"images"`
}
type ResultCost struct {
	Tool            string `json:"tool"`
	Timestamp       string `json:"timestamp"`
	EstimatedTokens int    `json:"estimatedTokens"`
}
type ImageCost struct {
	Tool            string `json:"tool"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	EstimatedTokens int    `json:"estimatedTokens"`
}
type HourCost struct {
	Hour         string `json:"hour"`
	Responses    int    `json:"responses"`
	FirstContext int    `json:"firstContext"`
	LastContext  int    `json:"lastContext"`
	Growth       int    `json:"growth"`
	Output       int    `json:"output"`
}
type ContextReport struct {
	Path             string       `json:"path"`
	Kind             string       `json:"kind"`
	Responses        int          `json:"responses"`
	MaxContext       int          `json:"maxContext"`
	OutputTokens     int          `json:"outputTokens"`
	ThinkingTokens   int          `json:"thinkingTokens"`
	TextTokens       int          `json:"estimatedAssistantTextTokens"`
	Tools            []ToolCost   `json:"tools"`
	TopResults       []ResultCost `json:"topResults"`
	Images           []ImageCost  `json:"images"`
	Hours            []HourCost   `json:"hours"`
	TodoInjections   int          `json:"sessionStartTodoInjections"`
	MalformedRecords int          `json:"malformedRecords"`
}

type transcriptBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    struct {
		Data string `json:"data"`
	} `json:"source"`
}
type transcriptRow struct {
	Attachment struct {
		Type      string `json:"type"`
		HookEvent string `json:"hookEvent"`
		Command   string `json:"command"`
	} `json:"attachment"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input    int `json:"input_tokens"`
			Creation int `json:"cache_creation_input_tokens"`
			Read     int `json:"cache_read_input_tokens"`
			Output   int `json:"output_tokens"`
			Details  struct {
				Thinking int `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

// ResolveTranscript uses filename prefixes only; it never searches transcript
// contents and refuses ambiguous session prefixes. latest means latest mtime.
// An explicit selector reaches agent transcripts too — see the walk below.
func ResolveTranscript(root, selector string) (string, error) {
	if selector == "" || strings.ContainsAny(selector, "/\\") {
		return "", fmt.Errorf("use a session id prefix or latest")
	}
	var matches []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// `latest` means the latest SESSION. Walking into agent transcripts makes
		// it resolve to whichever agent happened to write last — a few hundred
		// tokens of someone else's context, reported as yours. An explicit
		// selector asks a different question: it names one transcript, so agent
		// runs stay reachable. Skipping them there instead made 40% of the
		// transcript tree unmeasurable, which is where delegated context hides.
		if d.IsDir() && selector == "latest" && (d.Name() == "subagents" || d.Name() == "workflows") {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") && (selector == "latest" || strings.HasPrefix(d.Name(), selector)) {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no transcript matches %q", selector)
	}
	if selector != "latest" {
		if len(matches) != 1 {
			return "", fmt.Errorf("ambiguous session prefix %q (%d matches)", selector, len(matches))
		}
		return matches[0], nil
	}
	latest := matches[0]
	st, err := os.Stat(latest)
	if err != nil {
		return "", err
	}
	for _, p := range matches[1:] {
		next, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if next.ModTime().After(st.ModTime()) {
			latest, st = p, next
		}
	}
	return latest, nil
}

// transcriptKind labels a transcript by where it sits in the projects tree, so a
// caller aggregating a corpus never averages a lead session together with the
// agent runs it spawned. Workflow agents live under subagents/workflows, so the
// more specific label wins.
func transcriptKind(path string) string {
	kind := "session"
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		switch seg {
		case "workflows":
			return "workflow-agent"
		case "subagents":
			kind = "subagent"
		}
	}
	return kind
}

func DiagnoseContext(path string) (ContextReport, error) {
	r := ContextReport{Path: path, Kind: transcriptKind(path), Tools: []ToolCost{}, TopResults: []ResultCost{}, Images: []ImageCost{}, Hours: []HourCost{}}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	calls := map[string]string{}
	seen := map[string]bool{}
	tools := map[string]*ToolCost{}
	hours := map[string]*HourCost{}
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var row transcriptRow
			if err := json.Unmarshal(line, &row); err != nil {
				r.MalformedRecords++
			} else {
				if row.Type == "attachment" && row.Attachment.Type == "hook_success" && row.Attachment.HookEvent == "SessionStart" && strings.Contains(row.Attachment.Command, "hook-context todo") {
					r.TodoInjections++
				}
				u := row.Message.Usage
				if row.Type == "assistant" && u != nil && row.Message.ID != "" && !seen[row.Message.ID] {
					seen[row.Message.ID] = true
					r.Responses++
					r.OutputTokens += u.Output
					r.ThinkingTokens += u.Details.Thinking
					context := u.Input + u.Creation + u.Read
					r.MaxContext = max(r.MaxContext, context)
					if len(row.Timestamp) >= 13 {
						h := row.Timestamp[:13]
						if hours[h] == nil {
							hours[h] = &HourCost{Hour: h, FirstContext: context}
						}
						v := hours[h]
						v.Responses++
						v.LastContext = context
						v.Growth = context - v.FirstContext
						v.Output += u.Output
					}
				}
				blocks, err := contentBlocks(row.Message.Content)
				if err != nil {
					r.MalformedRecords++
				} else {
					for _, b := range blocks {
						switch b.Type {
						case "text":
							if row.Type == "assistant" {
								r.TextTokens += estimateTokens(b.Text)
							}
							if row.Type == "user" {
								r.TodoInjections += strings.Count(b.Text, "Open vitrinka todos")
							}
						case "tool_use":
							calls[b.ID] = b.Name
						case "tool_result":
							name := calls[b.ToolUseID]
							if name == "" {
								name = "unknown"
							}
							if tools[name] == nil {
								tools[name] = &ToolCost{Tool: name}
							}
							cost := tools[name]
							cost.Calls++
							parts, err := contentBlocks(b.Content)
							if err != nil {
								r.MalformedRecords++
								continue
							}
							tokens := 0
							for _, p := range parts {
								if p.Type == "image" {
									img := imageCost(p.Source.Data)
									img.Tool = name
									r.Images = append(r.Images, img)
									cost.Images++
								} else {
									tokens += estimateTokens(p.Text)
								}
							}
							cost.EstimatedTokens += tokens
							r.TopResults = append(r.TopResults, ResultCost{Tool: name, Timestamp: row.Timestamp, EstimatedTokens: tokens})
						}
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return r, readErr
		}
	}
	for _, cost := range tools {
		r.Tools = append(r.Tools, *cost)
	}
	sort.Slice(r.Tools, func(i, j int) bool {
		if r.Tools[i].EstimatedTokens == r.Tools[j].EstimatedTokens {
			return r.Tools[i].Tool < r.Tools[j].Tool
		}
		return r.Tools[i].EstimatedTokens > r.Tools[j].EstimatedTokens
	})
	sort.SliceStable(r.TopResults, func(i, j int) bool { return r.TopResults[i].EstimatedTokens > r.TopResults[j].EstimatedTokens })
	if len(r.TopResults) > 15 {
		r.TopResults = r.TopResults[:15]
	}
	for _, h := range hours {
		r.Hours = append(r.Hours, *h)
	}
	sort.Slice(r.Hours, func(i, j int) bool { return r.Hours[i].Hour < r.Hours[j].Hour })
	return r, nil
}

func contentBlocks(raw json.RawMessage) ([]transcriptBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		err := json.Unmarshal(raw, &s)
		return []transcriptBlock{{Type: "text", Text: s}}, err
	}
	var result []transcriptBlock
	err := json.Unmarshal(raw, &result)
	return result, err
}

func estimateTokens(s string) int { return int(math.Round(float64(len([]rune(s))) / 3.6)) }

// unknownImageTokens is charged to an image whose header we cannot read: the
// most any single image can cost once its long edge is scaled to 1568 px. It is
// deliberately an upper bound — the report counts these separately as "unknown
// dimensions" — because charging 0 understates the image total badly.
const unknownImageTokens = 1568 * 1568 / 750

// imageCost estimates one image block's token cost. Only PNG used to be read,
// which silently priced every screenshot in another format at zero.
func imageCost(data string) ImageCost {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	raw, err := base64.StdEncoding.DecodeString(head[:len(head)/4*4])
	if err != nil {
		return ImageCost{EstimatedTokens: unknownImageTokens}
	}
	w, h := imageDimensions(raw)
	if w <= 0 || h <= 0 {
		return ImageCost{EstimatedTokens: unknownImageTokens}
	}
	scale := math.Min(1, 1568/float64(max(w, h)))
	return ImageCost{Width: w, Height: h, EstimatedTokens: int(math.Round(float64(w) * float64(h) * scale * scale / 750))}
}

// imageDimensions reads width and height out of a container header, or (0, 0)
// for a format or truncation it cannot read.
func imageDimensions(raw []byte) (int, int) {
	switch {
	case len(raw) >= 24 && string(raw[:8]) == "\x89PNG\r\n\x1a\n":
		return int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24]))
	case len(raw) >= 10 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a"):
		return int(binary.LittleEndian.Uint16(raw[6:8])), int(binary.LittleEndian.Uint16(raw[8:10]))
	case len(raw) >= 16 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return webpDimensions(raw)
	case len(raw) >= 4 && raw[0] == 0xFF && raw[1] == 0xD8:
		return jpegDimensions(raw)
	}
	return 0, 0
}

// jpegDimensions walks marker segments to the first start-of-frame.
func jpegDimensions(raw []byte) (int, int) {
	for i := 2; i+9 < len(raw); {
		if raw[i] != 0xFF || raw[i+1] == 0xFF {
			i++
			continue
		}
		marker := raw[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2 // standalone markers carry no length
			continue
		}
		// SOF0..SOF15, minus the huffman/arithmetic/extension markers sharing the range.
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			return int(binary.BigEndian.Uint16(raw[i+7 : i+9])), int(binary.BigEndian.Uint16(raw[i+5 : i+7]))
		}
		length := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		if length < 2 {
			return 0, 0
		}
		i += 2 + length
	}
	return 0, 0
}

// webpDimensions reads whichever of the three WebP chunk headers is present.
func webpDimensions(raw []byte) (int, int) {
	switch string(raw[12:16]) {
	case "VP8X":
		if len(raw) >= 30 {
			w := int(raw[24]) | int(raw[25])<<8 | int(raw[26])<<16
			h := int(raw[27]) | int(raw[28])<<8 | int(raw[29])<<16
			return w + 1, h + 1
		}
	case "VP8 ":
		if len(raw) >= 30 {
			return int(binary.LittleEndian.Uint16(raw[26:28]) & 0x3FFF), int(binary.LittleEndian.Uint16(raw[28:30]) & 0x3FFF)
		}
	case "VP8L":
		if len(raw) >= 25 {
			b := binary.LittleEndian.Uint32(raw[21:25])
			return int(b&0x3FFF) + 1, int((b>>14)&0x3FFF) + 1
		}
	}
	return 0, 0
}

func (r ContextReport) Render(w io.Writer) error {
	imageTokens := 0
	unknown := 0
	for _, img := range r.Images {
		imageTokens += img.EstimatedTokens
		if img.Width == 0 {
			unknown++
		}
	}
	_, err := fmt.Fprintf(w, "%s [%s]\n%d responses · max context %d · own output %d (thinking %d)\n%d images ≈ %d tokens (%d unknown dimensions) · SessionStart todo injections %d · malformed records %d\nText/tool/image estimates are approximate; usage counts are recorded.\n", r.Path, r.Kind, r.Responses, r.MaxContext, r.OutputTokens, r.ThinkingTokens, len(r.Images), imageTokens, unknown, r.TodoInjections, r.MalformedRecords)
	if err != nil {
		return err
	}
	for _, t := range r.Tools {
		if _, err = fmt.Fprintf(w, "%5d calls %8d estimated tokens %3d images  %s\n", t.Calls, t.EstimatedTokens, t.Images, t.Tool); err != nil {
			return err
		}
	}
	for _, t := range r.TopResults {
		if _, err = fmt.Fprintf(w, "top %s %7d tokens %s\n", t.Timestamp, t.EstimatedTokens, t.Tool); err != nil {
			return err
		}
	}
	for _, h := range r.Hours {
		if _, err = fmt.Fprintf(w, "%s %4d responses context %d growth %+d output %d\n", h.Hour, h.Responses, h.LastContext, h.Growth, h.Output); err != nil {
			return err
		}
	}
	return nil
}
