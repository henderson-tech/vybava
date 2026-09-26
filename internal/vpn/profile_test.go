package vpn

import (
	"strings"
	"testing"
)

const keyA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
const keyB = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func fixture() string {
	return "[Interface]\nPrivateKey = " + keyA + "\nAddress = 10.8.1.2/32\n[Peer]\nPublicKey = " + keyB + "\nPresharedKey = " + keyA + "\nAllowedIPs = 10.8.1.0/24\nEndpoint = 192.0.2.1:51820\n"
}

func TestPrepareRejectsHooksAndMalformedKeysWithoutEchoing(t *testing.T) {
	for _, input := range []string{
		strings.Replace(fixture(), "Address =", "PostUp =", 1),
		strings.Replace(fixture(), "Address = 10.8.1.2/32", "Table = off", 1),
		strings.Replace(fixture(), keyA, "sensitive-invalid-key", 1),
	} {
		_, err := Prepare(input, nil)
		if err == nil {
			t.Fatalf("accepted %q", input)
		}
		if strings.Contains(err.Error(), "sensitive-invalid-key") {
			t.Fatal("leaked key")
		}
	}
}

func TestPrepareExcludesOnlyNamedPeer(t *testing.T) {
	input := fixture() + "[Peer]\nPublicKey = " + keyA + "\nAllowedIPs = 10.9.0.0/24\n"
	config, err := Prepare(input, []string{keyA})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(config, "[Peer]") != 1 || strings.Contains(config, "10.9.0.0") || !strings.Contains(config, "PrivateKey = "+keyA) {
		t.Fatalf("wrong peer excluded:\n%s", config)
	}
}

func TestSaveReplacesAndValidatesRegistration(t *testing.T) {
	dir := t.TempDir()
	p := Profile{Ref: "onyx://VPN/test/config", Probes: []string{"10.8.1.1:443"}}
	if err := Save(dir, "admin", p); err != nil {
		t.Fatal(err)
	}
	p.DNS = "10.8.1.1"
	if err := Save(dir, "admin", p); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir, "admin"); err != nil || got.DNS != "10.8.1.1" || dnsAddr(got.DNS) != "10.8.1.1:53" {
		t.Fatalf("load: %v %#v", err, got)
	}
	if err := Save(dir, "../escape", p); err == nil {
		t.Fatal("accepted unsafe name")
	}
	p.DNS = "vpn.example"
	if err := Save(dir, "admin", p); err == nil {
		t.Fatal("accepted a DNS server that is not an IP")
	}
}
