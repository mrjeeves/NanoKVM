package mesh

import "strings"

// Joining-mesh id derivation. Every KVM ships with its own **joining mesh** —
// the network an unclaimed/reset device sits on waiting to be adopted. Nothing
// is printed on a box: the id is derived deterministically from the daemon's
// device identity and surfaced on the device's screen and web UI, so whoever
// is standing in front of the hardware can read it, join it from AllMyStuff,
// and claim the device. Unclaiming ("reset to factory") returns the device to
// exactly this mesh.
//
// Shape: cec-kvm-<5 chars>-<5 chars>, 19 chars total — a valid MyOwnMesh
// network id (3–64 of [a-z0-9-_]). The five-char groups use a reduced,
// human-readable alphabet with the usual look-alikes removed (no 0/1/i/l/o),
// because people transcribe this id from a 128×32 OLED.
//
// Derivation mirrors fleet.go's style (FNV-1a64 forward + reversed for two
// independent digests) and is FROZEN: a device's joining mesh must never move
// across firmware upgrades, or the id on the box's screen yesterday stops
// being where the device reappears after a reset tomorrow.

// joiningMeshPrefix brands the id so a human (and the AllMyStuff UI) can tell
// a joining mesh from any other network at a glance.
const joiningMeshPrefix = "cec-kvm-"

// joiningAlphabet is the 31-char human-readable set: lowercase base36 minus
// the look-alikes 0/1/i/l/o.
const joiningAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

// DeriveJoiningMeshID derives this device's joining mesh from its daemon
// device id. The id is canonicalised first (display suffix stripped,
// lowercased) so every rendering of the same identity derives the same mesh.
func DeriveJoiningMeshID(deviceID string) string {
	id := strings.ToLower(pubkeyPart(strings.TrimSpace(deviceID)))
	h1 := fnv1a64([]byte(id))
	reversed := make([]byte, len(id))
	for i := 0; i < len(id); i++ {
		reversed[i] = id[len(id)-1-i]
	}
	h2 := fnv1a64(reversed)
	return joiningMeshPrefix + joiningGroup(h1) + "-" + joiningGroup(h2)
}

// joiningGroup renders n as 5 chars of the human-readable alphabet, low digit
// first — the same digit-extraction shape as fleet.go's base36.
func joiningGroup(n uint64) string {
	const width = 5
	out := make([]byte, 0, width)
	for i := 0; i < width; i++ {
		out = append(out, joiningAlphabet[n%uint64(len(joiningAlphabet))])
		n /= uint64(len(joiningAlphabet))
	}
	return string(out)
}

// IsJoiningMeshID reports whether a network id looks like a KVM joining mesh
// (the cec-kvm- brand). Cosmetic classification only — never an authorization
// input.
func IsJoiningMeshID(networkID string) bool {
	return strings.HasPrefix(networkID, joiningMeshPrefix)
}
