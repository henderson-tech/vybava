package cmuxgrid

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateTargetsFocusedWindowAndVerifiesGrid(t *testing.T) {
	for _, count := range []int{8, 1} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "cmux.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				for _, method := range []string{"system.identify", "workspace.create", "pane.list"} {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					var request struct {
						Method string          `json:"method"`
						Params json.RawMessage `json:"params"`
					}
					err = json.NewDecoder(conn).Decode(&request)
					if err != nil {
						conn.Close()
						done <- err
						return
					}
					if request.Method != method {
						t.Errorf("got %s; want %s", request.Method, method)
					}
					var result json.RawMessage
					switch method {
					case "system.identify":
						result = json.RawMessage(`{"focused":{"window_id":"foreground-window"}}`)
					case "workspace.create":
						var params struct {
							WindowID string `json:"window_id"`
							Layout   Node   `json:"layout"`
							Focus    bool   `json:"focus"`
						}
						if err := json.Unmarshal(request.Params, &params); err != nil {
							t.Error(err)
						}
						if params.WindowID != "foreground-window" || !params.Focus || len(params.Layout.Children) != 2 {
							t.Errorf("wrong create target/layout: %+v", params)
						}
						result = json.RawMessage(`{"workspace_id":"new-workspace","window_id":"foreground-window"}`)
					case "pane.list":
						var params struct {
							WorkspaceID string `json:"workspace_id"`
						}
						_ = json.Unmarshal(request.Params, &params)
						if params.WorkspaceID != "new-workspace" {
							t.Errorf("verified another workspace: %+v", params)
						}
						panes := make([]struct{}, count)
						result, err = json.Marshal(struct {
							Panes []struct{} `json:"panes"`
						}{panes})
					}
					if err == nil {
						err = json.NewEncoder(conn).Encode(struct {
							OK     bool            `json:"ok"`
							Result json.RawMessage `json:"result"`
						}{true, result})
					}
					conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			result, err := (Client{Socket: socket}).Create(context.Background(), Shape{4, 2})
			if (err != nil) != (count != 8) {
				t.Errorf("panes=%d result=%+v err=%v", count, result, err)
			}
			if result.WorkspaceID != "new-workspace" {
				t.Errorf("lost workspace receipt: %+v", result)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateStopsOnRPCFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		want      string
	}{
		{"rejected", []string{`{"ok":false,"error":{"code":"unauthorized","message":"access denied"}}`}, "unauthorized: access denied"},
		{"malformed", []string{`not-json`}, "response:"},
		{"no-focus", []string{`{"ok":true,"result":{"focused":{}}}`}, "no focused window"},
		{"no-receipt", []string{`{"ok":true,"result":{"focused":{"window_id":"w"}}}`, `{"ok":true,"result":{}}`}, "did not return the created workspace ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "cmux.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				for _, response := range tc.responses {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					var request json.RawMessage
					err = json.NewDecoder(conn).Decode(&request)
					if err == nil {
						_, err = conn.Write([]byte(response + "\n"))
					}
					conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				// No further request can succeed, so retrying/mutating after a
				// failed stage cannot accidentally make this test pass.
				listener.Close()
				done <- nil
			}()
			_, err = (Client{Socket: socket}).Create(context.Background(), Shape{4, 2})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %q", err, tc.want)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
