// Package onyxrest is one REST call with a vault-injected secret: Onyx's
// run_command puts the token in ONYX_REST_TOKEN, this process adds the auth
// header, and the response lands in a 0600 file (Onyx suppresses the stdout
// of every secret-injected child, so a file is the only channel back).
//
// The Onyx binding is an argv STRING PREFIX such as
// `/path/onyx-rest --base https://jira.example/`, so the argv grammar is
// strict: --base comes first and exactly once, and PATH can never leave the
// base's scheme and host. Bind with the trailing slash — without it the prefix
// also admits `https://jira.example.evil.test`.
package onyxrest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// TokenEnv is the only place the secret is read from.
const TokenEnv = "ONYX_REST_TOKEN"

// Timeout bounds one call end to end.
const Timeout = 60 * time.Second

// Usage is the one-line grammar printed with every usage error.
const Usage = "usage: onyx-rest --base https://<host>/ [--basic-user <user>] <METHOD> <PATH> [--data <json>|@<file>] --out <file>"

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// ErrUsage marks a refused invocation (exit 2); the message names the fix.
var ErrUsage = errors.New("usage")

// Request is a parsed, validated invocation.
type Request struct {
	Base      *url.URL
	BasicUser string
	Method    string
	URL       *url.URL
	Data      []byte
	Out       string
}

// Response is the --out file's shape.
type Response struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

func usagef(format string, args ...any) error {
	return fmt.Errorf("%w: %s — %s", ErrUsage, fmt.Sprintf(format, args...), Usage)
}

// Parse validates argv (without argv[0]). --base must be args[0] and appear
// once; every other flag may appear once, in any order around METHOD and PATH.
func Parse(args []string) (Request, error) {
	var req Request
	if len(args) < 2 || args[0] != "--base" {
		return req, usagef("--base <url> must be the first argument")
	}
	base, err := parseBase(args[1])
	if err != nil {
		return req, err
	}
	req.Base = base

	var positional []string
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--base" || strings.HasPrefix(arg, "--base="):
			return req, usagef("--base may appear only once, as the first argument")
		case arg == "--basic-user" || arg == "--data" || arg == "--out":
			if seen[arg] {
				return req, usagef("%s given twice", arg)
			}
			if i+1 >= len(args) {
				return req, usagef("%s needs a value", arg)
			}
			seen[arg] = true
			i++
			switch arg {
			case "--basic-user":
				req.BasicUser = args[i]
			case "--data":
				if req.Data, err = readData(args[i]); err != nil {
					return req, err
				}
			case "--out":
				req.Out = args[i]
			}
		case strings.HasPrefix(arg, "-"):
			return req, usagef("unknown flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 2 {
		return req, usagef("want METHOD and PATH, got %d positional arguments", len(positional))
	}
	if req.Out == "" {
		return req, usagef("--out <file> is required")
	}
	req.Method = strings.ToUpper(positional[0])
	if !methods[req.Method] {
		return req, usagef("method %q is not one of GET POST PUT PATCH DELETE", positional[0])
	}
	if req.URL, err = resolve(base, positional[1]); err != nil {
		return req, err
	}
	return req, nil
}

func parseBase(raw string) (*url.URL, error) {
	base, err := url.Parse(raw)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, usagef("--base %q must be an https://<host> URL", raw)
	}
	if base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, usagef("--base %q may carry nothing after the host but a trailing slash", raw)
	}
	return base, nil
}

func resolve(base *url.URL, path string) (*url.URL, error) {
	route, _, _ := strings.Cut(path, "?")
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(route, "://") || strings.Contains(route, `\`) {
		return nil, usagef("PATH %q must be a single-slash path on the base host", path)
	}
	ref, err := url.Parse(path)
	if err != nil || ref.Scheme != "" || ref.Host != "" || ref.User != nil {
		return nil, usagef("PATH %q must be a single-slash path on the base host", path)
	}
	full := base.ResolveReference(ref)
	if full.Scheme != base.Scheme || full.Host != base.Host {
		return nil, usagef("PATH %q resolves off the base host", path)
	}
	return full, nil
}

func readData(raw string) ([]byte, error) {
	data := []byte(raw)
	if file, ok := strings.CutPrefix(raw, "@"); ok {
		read, err := os.ReadFile(file)
		if err != nil {
			return nil, usagef("--data file %s is unreadable: %v", file, err)
		}
		data = read
	}
	if !json.Valid(data) {
		return nil, usagef("--data is not valid JSON")
	}
	return data, nil
}

// Do sends the request with token, writes --out and returns the HTTP status.
// A transport failure writes nothing.
func Do(ctx context.Context, client *http.Client, req Request, token string) (int, error) {
	if token == "" {
		return 0, usagef("%s is empty — inject the secret through onyx run_command env_refs", TokenEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	var body io.Reader
	if req.Data != nil {
		body = bytes.NewReader(req.Data)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), body)
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if req.Data != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.BasicUser != "" {
		httpReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(req.BasicUser+":"+token)))
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := client.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("%s %s failed: %w", req.Method, req.URL.Redacted(), unwrapURL(err))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, fmt.Errorf("reading the %s response: %w", req.URL.Host, err)
	}

	out := Response{Status: res.StatusCode}
	if len(bytes.TrimSpace(raw)) > 0 {
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			out.Body = parsed
		} else {
			out.Body = string(raw)
		}
	}
	return res.StatusCode, writeOut(req.Out, out)
}

func unwrapURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func writeOut(path string, out Response) error {
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing --out: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("writing --out: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("writing --out: %w", err)
	}
	return file.Close()
}
