package cmux

import (
	"context"
	"fmt"
)

// Capabilities is system.capabilities: what this cmux speaks and who may
// call it.
type Capabilities struct {
	AccessMode   string   `json:"access_mode"`
	Protocol     string   `json:"protocol"`
	SocketPath   string   `json:"socket_path"`
	Version      int      `json:"version"`
	Methods      []string `json:"methods"`
	Capabilities []string `json:"capabilities"`
}

// Capabilities asks cmux what it supports.
func (c Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var caps Capabilities
	err := c.Call(ctx, "system.capabilities", nil, &caps)
	return caps, err
}

// Identity is the part of system.identify this client reads.
type Identity struct {
	BundlePath string `json:"app_bundle_path"`
	CLIPath    string `json:"app_cli_path"`
}

// Identify asks cmux where its app lives.
func (c Client) Identify(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.Call(ctx, "system.identify", nil, &id)
	return id, err
}

// Target is the surface hosting an agent process, resolved live.
type Target struct {
	SurfaceID   string `json:"surface_id"`
	WorkspaceID string `json:"workspace_id"`
	// Resolution is cmux's confidence in the pid → surface join
	// (`corroborated` when the pid and the surface's own records agree).
	Resolution string `json:"pid_resolution"`
}

// Resolve finds the surface whose terminal runs pid. A pid no surface
// hosts answers ErrNotFound.
func (c Client) Resolve(ctx context.Context, pid int) (Target, error) {
	var target Target
	if err := c.Call(ctx, "agent.resolve_delivery_target", map[string]int{"pid": pid}, &target); err != nil {
		return target, err
	}
	if target.SurfaceID == "" {
		return target, &Error{Method: "agent.resolve_delivery_target", Code: "not_found", Message: fmt.Sprintf("no surface for pid %d", pid)}
	}
	return target, nil
}

// Screen is a surface's terminal text with where it lives.
type Screen struct {
	Text        string `json:"text"`
	SurfaceID   string `json:"surface_id"`
	WorkspaceID string `json:"workspace_id"`
	WindowID    string `json:"window_id"`
}

// ReadText reads a surface's visible screen; lines > 0 reads that many
// trailing lines including scrollback instead.
func (c Client) ReadText(ctx context.Context, surfaceID string, lines int) (Screen, error) {
	params := map[string]any{"surface_id": surfaceID}
	if lines > 0 {
		params["lines"], params["scrollback"] = lines, true
	}
	var screen Screen
	err := c.Call(ctx, "surface.read_text", params, &screen)
	return screen, err
}

// PasteResult is terminal.paste's answer.
type PasteResult struct {
	Submitted   bool   `json:"submitted"`
	SubmitError string `json:"submit_error"`
	// Delivery is "queued" when the terminal was still starting.
	Delivery string `json:"delivery"`
}

// Paste puts text at the surface's prompt through the Cmd+V paste path and,
// when submit is true, presses the agent-aware submit key. A paste cmux
// accepted is never re-sent: Submitted false with the text delivered is a
// warning for the caller, not a reason to retry.
func (c Client) Paste(ctx context.Context, surfaceID, text string, submit bool) (PasteResult, error) {
	key := "none"
	if submit {
		key = "return"
	}
	var result PasteResult
	err := c.Call(ctx, "terminal.paste", map[string]string{"surface_id": surfaceID, "text": text, "submit_key": key}, &result)
	return result, err
}

// SendText types text into a surface as keystrokes — how a dialog option
// is picked: its digit. (surface.send_key knows named keys only, no digits.)
func (c Client) SendText(ctx context.Context, surfaceID, text string) error {
	return c.Call(ctx, "surface.send_text", map[string]string{"surface_id": surfaceID, "text": text}, nil)
}

// Focus brings a surface to the front: its window, its workspace, then the
// surface itself.
func (c Client) Focus(ctx context.Context, windowID, workspaceID, surfaceID string) error {
	if windowID != "" {
		if err := c.Call(ctx, "window.focus", map[string]string{"window_id": windowID}, nil); err != nil {
			return err
		}
	}
	if err := c.Call(ctx, "workspace.select", map[string]string{"workspace_id": workspaceID}, nil); err != nil {
		return err
	}
	return c.Call(ctx, "surface.focus", map[string]string{"surface_id": surfaceID}, nil)
}
