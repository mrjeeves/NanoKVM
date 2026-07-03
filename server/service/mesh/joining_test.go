package mesh

import (
	"regexp"
	"testing"
)

// TestDeriveJoiningMeshIDShape pins the wire-visible shape: the cec-kvm-
// brand, two 5-char human-readable groups, and validity as a MyOwnMesh
// network id.
func TestDeriveJoiningMeshIDShape(t *testing.T) {
	shape := regexp.MustCompile(`^cec-kvm-[23456789abcdefghjkmnpqrstuvwxyz]{5}-[23456789abcdefghjkmnpqrstuvwxyz]{5}$`)
	for _, id := range []string{"kvm-abcdef", "a", "some-very-long-pubkey-base32-string"} {
		got := DeriveJoiningMeshID(id)
		if !shape.MatchString(got) {
			t.Errorf("DeriveJoiningMeshID(%q) = %q, want cec-kvm-xxxxx-xxxxx from the readable alphabet", id, got)
		}
		if _, err := normalizeNetworkID(got); err != nil {
			t.Errorf("DeriveJoiningMeshID(%q) = %q is not a valid network id: %s", id, got, err)
		}
	}
}

// TestDeriveJoiningMeshIDCanonicalizes: every rendering of one identity —
// cased, display-suffixed, padded — derives the same joining mesh, or the id
// on the screen yesterday isn't where the device reappears tomorrow.
func TestDeriveJoiningMeshIDCanonicalizes(t *testing.T) {
	base := DeriveJoiningMeshID("abcdef")
	for _, alias := range []string{"ABCDEF", " abcdef ", "abcdef-XY3ZW", "ABCDEF-xy3zw"} {
		if got := DeriveJoiningMeshID(alias); got != base {
			t.Errorf("DeriveJoiningMeshID(%q) = %q, want %q (canonical of abcdef)", alias, got, base)
		}
	}
	if DeriveJoiningMeshID("abcdef") == DeriveJoiningMeshID("abcdeg") {
		t.Error("distinct identities derived the same joining mesh")
	}
}

// TestDeriveJoiningMeshIDFrozen pins exact outputs. The derivation is FROZEN:
// a firmware upgrade must never move a device's joining mesh. If this test
// fails, you changed the derivation — don't update the expectations, revert
// the change.
func TestDeriveJoiningMeshIDFrozen(t *testing.T) {
	cases := map[string]string{
		"kvm-abcdef": "cec-kvm-bgz2f-b6v8n",
		"abcdef":     "cec-kvm-s4p24-t2gnh",
	}
	for id, want := range cases {
		if got := DeriveJoiningMeshID(id); got != want {
			t.Errorf("DeriveJoiningMeshID(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestIsJoiningMeshID classifies by the brand prefix.
func TestIsJoiningMeshID(t *testing.T) {
	if !IsJoiningMeshID(DeriveJoiningMeshID("x1")) {
		t.Error("derived id not recognised as a joining mesh")
	}
	if IsJoiningMeshID("amber-turing-x3k9q") {
		t.Error("fleet-shaped id misread as a joining mesh")
	}
}
