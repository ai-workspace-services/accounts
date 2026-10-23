# Standalone migratectl release assets

The release packaging job builds `accounts` and its existing zip plus standalone
`migratectl-linux-amd64`, `migratectl-linux-arm64`, `migratectl-darwin-amd64` and
`migratectl-darwin-arm64`. Linux is the supported host migration runner. Windows
is not advertised: migration staging uses Unix file ownership/locking semantics.

All binaries are built from the clean checkout matching full `GITHUB_SHA`. The
same publication step hashes actual bytes and publishes `SHA256SUMS` and
`release-manifest.json`. Existing `files` names remain available; additive
`artifacts` entries contain `name`, `sha256`, and `size_bytes`. The manifest also
records the source commit, release tag, image refs and chart refs. SHA256SUMS
covers payload assets; the manifest additionally records SHA256SUMS' own digest.
Neither checksum file can checksum itself.

The release owner must verify the immutable tag/source SHA and trusted release
manifest before installing an asset. Verify its exact SHA-256 against both the
manifest and SHA256SUMS, install it as a protected regular executable, then set
`MIGRATECTL_BIN` to that absolute path and `MIGRATECTL_SHA256` to the manifest's
expected asset digest. The public caller rechecks those bytes before every exec.
Do not substitute a checksum computed from an unverified download as the trusted
expected value. No release was published by the local packaging tests.

This adds release artifacts only. Image identity, registry selection, deployment
events, environments and runtime `/api/ping` behavior remain unchanged. Consumers
must pin a newly reviewed release containing these assets; historical releases
without migratectl/checksums are not silently accepted or modified.
