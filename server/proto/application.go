package proto

type GetVersionRsp struct {
	// LatestError is why the release-channel lookup failed, when it did. Empty
	// on success. Carried so the UI can say "couldn't check" — and say why —
	// instead of the reassuring lie an empty Latest used to produce.
	LatestError string `json:"latestError"`

	Current string `json:"current"`
	Latest  string `json:"latest"`
}

type GetPreviewRsp struct {
	Enabled bool `json:"enabled"`
}

type SetPreviewReq struct {
	Enable bool `validate:"omitempty"`
}
