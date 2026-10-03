package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Fleet.app builds in Swift 6 with MainActor default isolation; a generated
// type that is not nonisolated cannot satisfy FleetEnvelope's Sendable
// parameter there. Type-check the contract the way the app compiles it.
func TestSwiftTypeChecksUnderMainActorDefault(t *testing.T) {
	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		t.Skip("swiftc not installed")
	}
	dir := t.TempDir()
	contract := filepath.Join(dir, "FleetSnapshot.swift")
	use := filepath.Join(dir, "Use.swift")
	if err := os.WriteFile(contract, []byte(Swift()), 0o644); err != nil {
		t.Fatal(err)
	}
	const usage = `import Foundation

func decodeAll(_ data: Data) throws {
    let decoder = FleetJSON.decoder()
    _ = try decoder.decode(FleetSnapshot.self, from: data)
    _ = try decoder.decode(FleetEnvelope<FleetScreen>.self, from: data)
    _ = try decoder.decode(FleetEnvelope<FleetReply>.self, from: data)
    _ = try decoder.decode(FleetEnvelope<FleetFocus>.self, from: data)
}

func decodeOffMain(_ data: Data) async throws -> FleetEnvelope<FleetScreen> {
    try await Task.detached { try FleetJSON.decoder().decode(FleetEnvelope<FleetScreen>.self, from: data) }.value
}
`
	if err := os.WriteFile(use, []byte(usage), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(swiftc, "-typecheck", "-swift-version", "6", "-default-isolation", "MainActor", "-warnings-as-errors", contract, use)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated Swift does not type-check as Fleet.app compiles it: %v\n%s", err, out)
	}
}
