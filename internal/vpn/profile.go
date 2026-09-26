// Package vpn runs named macOS WireGuard tunnels outside WireGuard.app as
// persistent LaunchDaemons, and reports honestly how each one is carried.
//
// The tunnel's profile (the WireGuard config with its private key) lives in
// Onyx; `install` pulls it through an Onyx-injected child and a private FIFO
// into a root-only file the daemon reads at boot. The non-secret registration
// (vault reference, probes, DNS server) lives in ~/.config/vybava/vpn.
// Docs: docs/vpn.md.
package vpn

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SecretEnv carries the vault profile into the Onyx-injected child only.
const SecretEnv = "VYBAVA_WG_PROFILE"

// maxConfig bounds a profile; a WireGuard config with a handful of peers is
// well under 1 KiB.
const maxConfig = 16 << 10

// A tunnel name is also the wg-quick interface name, which macOS caps at 15.
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

// Profile is the non-secret registration of one tunnel.
type Profile struct {
	// Ref is the Onyx reference holding the full WireGuard config.
	Ref string `json:"ref"`
	// Probes are TCP host:port targets that prove the tunnel carries traffic.
	Probes []string `json:"probes"`
	// DNS is the resolver the tunnel carries (host or host:port); status asks
	// it a question, because a dead tunnel DNS stalls every lookup the Mac's
	// /etc/resolver files send to it.
	DNS string `json:"dns,omitempty"`
	// ExcludePeers are retired peer public keys omitted from the vault copy.
	ExcludePeers []string `json:"excludePeers,omitempty"`
}

// Directory is where profiles are registered.
func Directory() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "vybava", "vpn"), err
}

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return errors.New("tunnel name must be 1–15 letters, digits, dots, underscores or hyphens, starting with a letter or digit")
	}
	return nil
}

// Names lists the registered profiles, sorted.
func Names(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, m := range matches {
		if name := strings.TrimSuffix(filepath.Base(m), ".json"); ValidateName(name) == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

// Read returns the registration as stored, unvalidated, so `add` can repair
// it; found is false when none exists.
func Read(dir, name string) (p Profile, found bool, err error) {
	if err := ValidateName(name); err != nil {
		return p, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, true, fmt.Errorf("profile %s: %w", name, err)
	}
	return p, true, nil
}

func Load(dir, name string) (Profile, error) {
	p, found, err := Read(dir, name)
	if err != nil {
		return p, err
	}
	if !found {
		return p, fmt.Errorf("no tunnel %s is registered in %s", name, dir)
	}
	return p, p.validate()
}

func (p Profile) validate() error {
	if !strings.HasPrefix(p.Ref, "onyx://") {
		return errors.New("profile requires an onyx:// configuration reference (--ref)")
	}
	if len(p.Probes) == 0 {
		return errors.New("at least one --probe host:port is required")
	}
	for _, probe := range p.Probes {
		if _, _, err := net.SplitHostPort(probe); err != nil {
			return fmt.Errorf("invalid probe %q: %w", probe, err)
		}
	}
	if p.DNS != "" {
		if _, err := netip.ParseAddr(dnsHost(p.DNS)); err != nil {
			return fmt.Errorf("invalid --dns %q: want an IP address, optionally with :port", p.DNS)
		}
	}
	return nil
}

// Save writes the registration atomically; the file holds no key material.
func Save(dir, name string, p Profile) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := p.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.json")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(append(b, '\n'))
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name+".json"))
}

// dnsHost strips an optional port from a DNS server spec.
func dnsHost(spec string) string {
	if host, _, err := net.SplitHostPort(spec); err == nil {
		return host
	}
	return spec
}

// dnsAddr is the DNS server spec as host:port.
func dnsAddr(spec string) string {
	if _, _, err := net.SplitHostPort(spec); err == nil {
		return spec
	}
	return net.JoinHostPort(spec, "53")
}

// Prepare validates a vault profile, drops explicitly retired peers and
// rejects everything wg-quick would execute (hooks, SaveConfig, Table): the
// result is run by root at every boot. Errors never quote input lines — they
// may hold private keys.
func Prepare(raw string, excluded []string) (string, error) {
	type section struct {
		kind  string
		lines []string
		key   string
	}
	var sections []section
	interfaceCount, privateCount := 0, 0
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(strings.SplitN(rawLine, "#", 2)[0])
		if line == "" {
			continue
		}
		if line == "[Interface]" || line == "[Peer]" {
			sections = append(sections, section{kind: line})
			if line == "[Interface]" {
				interfaceCount++
			}
			continue
		}
		if len(sections) == 0 {
			return "", errors.New("configuration must begin with a section")
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			return "", errors.New("invalid WireGuard configuration line")
		}
		key, value := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		s := &sections[len(sections)-1]
		allowed := map[string]bool{"PrivateKey": true, "Address": true, "ListenPort": true, "MTU": true, "DNS": true}
		if s.kind == "[Peer]" {
			allowed = map[string]bool{"PublicKey": true, "PresharedKey": true, "AllowedIPs": true, "Endpoint": true, "PersistentKeepalive": true}
		}
		if !allowed[key] || value == "" {
			return "", errors.New("unsupported or empty WireGuard setting; hooks, SaveConfig and custom route tables are not allowed")
		}
		if key == "PrivateKey" || key == "PublicKey" || key == "PresharedKey" {
			b, e := base64.StdEncoding.DecodeString(value)
			if e != nil || len(b) != 32 {
				return "", errors.New("invalid WireGuard key encoding")
			}
		}
		if key == "Address" || key == "AllowedIPs" {
			for _, prefix := range strings.Split(value, ",") {
				if _, e := netip.ParsePrefix(strings.TrimSpace(prefix)); e != nil {
					return "", errors.New("invalid WireGuard network prefix")
				}
			}
		}
		if key == "PrivateKey" {
			privateCount++
		}
		if key == "PublicKey" {
			s.key = value
		}
		s.lines = append(s.lines, key+" = "+value)
	}
	if interfaceCount != 1 || privateCount != 1 || len(sections) < 2 || sections[0].kind != "[Interface]" {
		return "", errors.New("configuration requires one interface/private key and at least one peer")
	}
	var out strings.Builder
	peers := 0
	for _, s := range sections {
		if s.kind == "[Peer]" {
			if s.key == "" {
				return "", errors.New("peer has no public key")
			}
			retired := false
			for _, key := range excluded {
				retired = retired || s.key == key
			}
			if retired {
				continue
			}
			peers++
		}
		out.WriteString(s.kind + "\n")
		for _, line := range s.lines {
			out.WriteString(line + "\n")
		}
	}
	if peers == 0 {
		return "", errors.New("no peers remain")
	}
	return out.String(), nil
}
