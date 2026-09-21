package cmuxgrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// CLI is used only for its installed version; all mutations explicitly target
// the foreground window, never the launching terminal's CMUX_WORKSPACE_ID.
func CheckVersion(ctx context.Context, cli string) error {
	out, err := exec.CommandContext(ctx, cli, "--version").Output()
	if err != nil {
		return fmt.Errorf("read cmux version: %w", err)
	}
	m := regexp.MustCompile(`cmux (\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if len(m) != 4 {
		return fmt.Errorf("unrecognized cmux version: %q", out)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	if major == 0 && (minor < 64 || minor == 64 && patch < 25) {
		return fmt.Errorf("%s needs updating to 0.64.25 or newer for native grid layouts and pane zoom; restart cmux after updating", m[0])
	}
	return nil
}

type Client struct{ Socket string }

type requestParams struct {
	WindowID    string `json:"window_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Focus       bool   `json:"focus,omitempty"`
	Layout      *Node  `json:"layout,omitempty"`
}

func (c Client) call(ctx context.Context, method string, params requestParams) (json.RawMessage, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, fmt.Errorf("connect to cmux: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	request := struct {
		ID     string        `json:"id"`
		Method string        `json:"method"`
		Params requestParams `json:"params"`
	}{"cmux-grid", method, params}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&response); err != nil {
		return nil, fmt.Errorf("cmux %s response: %w (external clients need Settings → Socket Control → Automation mode)", method, err)
	}
	if !response.OK {
		return nil, fmt.Errorf("cmux %s: %s: %s", method, response.Error.Code, response.Error.Message)
	}
	return response.Result, nil
}

type Result struct {
	WorkspaceID string `json:"workspace_id"`
	WindowID    string `json:"window_id"`
	Shape       Shape  `json:"shape"`
}

func (c Client) Create(ctx context.Context, shape Shape) (Result, error) {
	layout, err := Layout(shape)
	if err != nil {
		return Result{}, err
	}
	var identity struct {
		Focused struct {
			WindowID string `json:"window_id"`
		} `json:"focused"`
	}
	raw, err := c.call(ctx, "system.identify", requestParams{})
	if err != nil {
		return Result{}, err
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return Result{}, err
	}
	if identity.Focused.WindowID == "" {
		return Result{}, fmt.Errorf("cmux has no focused window")
	}
	params := requestParams{WindowID: identity.Focused.WindowID, Title: fmt.Sprintf("Grid %d×%d", shape.Columns, shape.Rows), Focus: true, Layout: &layout}
	var result Result
	raw, err = c.call(ctx, "workspace.create", params)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	result.Shape = shape
	if result.WorkspaceID == "" {
		return result, fmt.Errorf("cmux did not return the created workspace ID; inspect cmux before retrying")
	}
	var panes struct {
		Panes []json.RawMessage `json:"panes"`
	}
	raw, err = c.call(ctx, "pane.list", requestParams{WorkspaceID: result.WorkspaceID})
	if err != nil {
		return result, fmt.Errorf("workspace %s created but verification failed: %w", result.WorkspaceID, err)
	}
	if err := json.Unmarshal(raw, &panes); err != nil {
		return result, err
	}
	if len(panes.Panes) != shape.Columns*shape.Rows {
		return result, fmt.Errorf("workspace %s has %d panes, expected %d; ensure the running app was restarted after updating (workspace kept)", result.WorkspaceID, len(panes.Panes), shape.Columns*shape.Rows)
	}
	return result, nil
}
