// Package buildinfo is the single source of truth for our fork's release
// version. Both the mesh presence advert (server/service/mesh) and the
// firmware updater (server/service/application) read Version from here, so the
// number we advertise and the number the updater compares against are always
// the same — OUR fork's version, never the Sipeed base image's /kvmapp/version
// (which we don't own and which reads as an unrelated upstream 2.x).
//
// Bumped by scripts/bump-version.sh (which also bumps web/package.json).
package buildinfo

// Version is the fork's release version, e.g. "0.1.0". Kept in sync with the
// vX.Y.Z release tags.
const Version = "0.1.0"
