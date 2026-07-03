package mesh

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// Claim-code rendezvous — the WAN claim path behind config.Mesh.PublicClaims.
//
// While an unclaimed KVM has public claims enabled it mints a random claim
// code and joins the randomized network derived from it; the owner types the
// code into AllMyStuff's Fleet pane ("Claim a remote device") and both sides
// meet there. Unlike the on-screen joining-mesh id (deterministic per device),
// the code is random and rotates after every successful claim — a code that
// admitted an owner is spent.
//
// FROZEN: the encoding and the network-id derivation mirror
// AllMyStuff's `allmystuff-protocol` (`claim_code_from_bytes` /
// `claim_code_network_id`) byte for byte — both sides must land in the same
// signaling room from the same code.

// claimCodeBytes is the code's entropy: 16 bytes → 26 base32 chars.
const claimCodeBytes = 16

// claimCodePrefix brands the rendezvous network id.
const claimCodePrefix = "amsclaim-"

// claimCodeFromBytes renders code bytes as lowercase RFC 4648 base32, no
// padding — the exact encoding AllMyStuff produces.
func claimCodeFromBytes(b []byte) string {
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// newClaimCode mints a fresh random claim code.
func newClaimCode() string {
	b := make([]byte, claimCodeBytes)
	// crypto/rand failing is catastrophic and effectively impossible; a
	// predictable claim code would be a guessable rendezvous, so fail loudly
	// rather than mint one.
	if _, err := rand.Read(b); err != nil {
		panic("system RNG unavailable for claim code: " + err.Error())
	}
	return claimCodeFromBytes(b)
}

// claimCodeNetworkID derives the rendezvous network id from a claim code,
// normalizing display formatting (dash groups, case) away so the claimer's
// typed form and our raw form derive the same room.
func claimCodeNetworkID(code string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(code) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return claimCodePrefix + b.String()
}

// formatClaimCode renders a code in its human display form: dash groups of
// four (xxxx-xxxx-…). Purely cosmetic; claimCodeNetworkID strips it again.
func formatClaimCode(code string) string {
	var groups []string
	for i := 0; i < len(code); i += 4 {
		end := i + 4
		if end > len(code) {
			end = len(code)
		}
		groups = append(groups, code[i:end])
	}
	return strings.Join(groups, "-")
}
