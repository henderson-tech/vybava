package vpn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type onyxRequest struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      int        `json:"id"`
	Method  string     `json:"method"`
	Params  onyxParams `json:"params"`
}
type onyxParams struct {
	Name      string            `json:"name"`
	Arguments onyxArguments     `json:"arguments"`
	Meta      map[string]string `json:"_meta"`
}
type onyxArguments struct {
	Argv    []string          `json:"argv"`
	EnvRefs map[string]string `json:"env_refs"`
}

// ApplyArgv is the Onyx-injected child's command line. The vault item's
// allowed_commands binds its prefix: `<resolved exe> vpn _apply <name>`.
func ApplyArgv(exe, name, dir, fifo string) []string {
	return []string{exe, "vpn", "_apply", name, dir, fifo}
}

// Deliver is the Onyx-injected child: it validates the injected profile and
// writes it into the parent's FIFO, refusing any other kind of file.
func Deliver(dir, name, fifo string) error {
	p, err := Load(dir, name)
	if err != nil {
		return err
	}
	config, err := Prepare(os.Getenv(SecretEnv), p.ExcludePeers)
	os.Unsetenv(SecretEnv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("refusing to write the profile anywhere but the installer's pipe")
	}
	_, err = f.WriteString(config)
	return err
}

// Invoke asks the resident Onyx MCP daemon to inject the profile into a child.
// The loopback token authenticates transport; it is never returned or logged.
func Invoke(ctx context.Context, argv []string, ref string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := os.Getenv("ONYX_MCP_HTTP_TOKEN_FILE")
	if path == "" {
		path = filepath.Join(home, "Library", "Application Support", "Onyx", "mcp-http-token")
	}
	token, err := os.ReadFile(path)
	if err != nil {
		return errors.New("Onyx token unavailable; open Onyx and start its MCP service")
	}
	port := "3212"
	if value := os.Getenv("ONYX_MCP_HTTP_PORT"); value != "" {
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 65535 {
			return errors.New("invalid ONYX_MCP_HTTP_PORT")
		}
		port = value
	}
	const protocol = "2026-07-28"
	body, err := json.Marshal(onyxRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: onyxParams{Name: "run_command", Arguments: onyxArguments{Argv: argv, EnvRefs: map[string]string{SecretEnv: ref}}, Meta: map[string]string{"io.modelcontextprotocol/protocolVersion": protocol}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+port+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocol)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "run_command")
	client := &http.Client{Timeout: 6 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Onyx request failed (open Onyx and unlock it): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Onyx HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("Onyx: %s", envelope.Error.Message)
	}
	for _, c := range envelope.Result.Content {
		if c.Type != "text" {
			continue
		}
		if envelope.Result.IsError {
			return fmt.Errorf("Onyx: %s", c.Text)
		}
		var result struct {
			Exit *int `json:"exit"`
		}
		if json.Unmarshal([]byte(c.Text), &result) == nil && result.Exit != nil {
			if *result.Exit != 0 {
				return fmt.Errorf("the vault profile was refused (vpn _apply exited %d); check it is a plain WireGuard config without hooks", *result.Exit)
			}
			return nil
		}
	}
	return errors.New("Onyx returned no command exit status")
}
