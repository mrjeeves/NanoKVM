package mesh

// Fleet-network id derivation — a byte-for-byte Go port of AllMyStuff's
// derive_fleet_network_id (node/src/ownership.rs). A fleet's closed-network id
// is derived deterministically from the shared fleet key so every co-owned
// device computes the SAME id without being told it. Mirroring it here lets the
// KVM truly join its owner's base fleet network on FleetKey handoff.
//
// FROZEN: the word-lists and the derivation must stay identical to the Rust side
// or a KVM would derive a different (stale) network id and never converge with
// its fleet. If the Rust lists ever change, mirror the change here.

// fleetAdjectives mirrors FLEET_ADJECTIVES in ownership.rs (frozen order).
var fleetAdjectives = []string{
	"amber", "ancient", "autumn", "bold", "brave", "bright", "brisk", "calm", "clever", "cobalt",
	"cosmic", "crimson", "daring", "dawn", "dusky", "eager", "elder", "ember", "fabled", "fancy",
	"fleet", "frosty", "gentle", "gilded", "golden", "hardy", "hidden", "humble", "ivory", "jolly",
	"keen", "lively", "lucky", "mellow", "merry", "mighty", "nimble", "noble", "polar", "quiet",
	"rapid", "royal", "rugged", "silent", "solar", "spry", "stout", "sunny", "swift", "tidal",
	"vivid", "wily",
}

// fleetNames mirrors FLEET_NAMES in ownership.rs (frozen order).
var fleetNames = []string{
	"ampere", "archimedes", "babbage", "bardeen", "bell", "bohr", "boyle", "carson", "curie",
	"dalton", "darwin", "dijkstra", "edison", "einstein", "euclid", "euler", "faraday", "fermi",
	"feynman", "franklin", "galileo", "gauss", "hawking", "heisenberg", "hertz", "hopper", "hubble",
	"joule", "kepler", "knuth", "lamarr", "lovelace", "maxwell", "meitner", "mendel", "morse",
	"newton", "noether", "nobel", "pascal", "pasteur", "planck", "ramanujan", "sagan", "tesla",
	"turing", "volta", "watt",
}

// DeriveFleetNetworkID derives the fleet's closed-network id from its key,
// matching ownership.rs::derive_fleet_network_id exactly: FNV-1a64 over the key
// picks adjective + name; FNV-1a64 over the reversed key gives a 5-char base36
// suffix. Lowercase alphanumerics + '-', a valid MyOwnMesh network id.
func DeriveFleetNetworkID(key string) string {
	h1 := fnv1a64([]byte(key))
	// Independent bits for the suffix from a digest over the reversed key.
	kb := []byte(key)
	reversed := make([]byte, len(kb))
	for i := range kb {
		reversed[i] = kb[len(kb)-1-i]
	}
	h2 := fnv1a64(reversed)

	adjective := fleetAdjectives[h1%uint64(len(fleetAdjectives))]
	// Shift before the modulo so name doesn't correlate with adjective.
	name := fleetNames[(h1>>21)%uint64(len(fleetNames))]
	return adjective + "-" + name + "-" + base36(h2, 5)
}

// fnv1a64 is the 64-bit FNV-1a hash, matching ownership.rs::fnv1a64.
func fnv1a64(bytes []byte) uint64 {
	const (
		offset uint64 = 0xcbf29ce484222325
		prime  uint64 = 0x00000100000001b3
	)
	hash := offset
	for _, b := range bytes {
		hash ^= uint64(b)
		hash *= prime // wrapping multiply (uint64 overflow wraps in Go)
	}
	return hash
}

// base36 renders n as width lowercase base36 chars, low digit first — matching
// ownership.rs::base36.
func base36(n uint64, width int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 0, width)
	for i := 0; i < width; i++ {
		out = append(out, digits[n%36])
		n /= 36
	}
	return string(out)
}
