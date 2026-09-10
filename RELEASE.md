# RMMWay Release Process

This document describes the process for creating and publishing an RMMWay
release. Follow these steps for every release, including patches.

## Prerequisites

- Go 1.24+ toolchain
- Docker and Docker Compose
- minisign and cosign installed
- Write access to the GitHub repository
- Write access to GitHub Container Registry (`ghcr.io`)

## Release Checklist

### 1. Prepare the Release

```sh
# Update version constant in server
# Edit server/cmd/server/main.go: change "0.1.0" to the new version

# Update .env.prod.example
# Edit .env.prod.example: update RMMWAY_VERSION

# Write release notes
# Edit docs/releases/vX.Y.Z.md with feature summary

# Update CHANGELOG.md
# Document all changes since the previous release
```

### 2. Build Everything

```sh
# Full build
make build

# All tests
make test

# Agent verification
make verify-agent

# Frontend build
cd frontend && npm run build && cd ..
```

### 3. Cross-Compile Agent Binaries

```sh
VERSION=X.Y.Z make agent
```

This produces static binaries for:

- Linux amd64, arm64
- Windows amd64, arm64
- macOS amd64, arm64

### 4. Sign Artifacts

```sh
# Sign agent binaries with minisign
MINISIGN_PASS=password VERSION=X.Y.Z make sign

# Verify signatures
make verify-sigs

# Sign Docker image with cosign
docker build -t rmmway:${VERSION} -f server/Dockerfile .
docker push ghcr.io/welcometotheweb/rmmway:${VERSION}
cosign sign --key env:AWS_PRIVATE_KEY ghcr.io/welcometotheweb/rmmway:${VERSION}
```

### 5. Generate SBOMs

```sh
# Generate CycloneDX SBOMs for all agent binaries
VERSION=X.Y.Z make sbom
```

### 6. Tag and Push

```sh
git tag -a v${VERSION} -m "Release ${VERSION}"
git push origin main
git push origin v${VERSION}
```

### 7. Publish Release on GitHub

Create a GitHub release with:

- All signed agent binaries
- Checksums file
- SBOM files
- Docker image tags
- Release notes

## Release Artifacts

Every release must include:

| Artifact | Description |
| -------- | ----------- |
| Agent binaries (5 platforms) | Static Linux/Windows/macOS executables |
| Agent minisig signatures | Per-binary cryptographic signatures |
| Checksums file | SHA-256 hashes for all binaries |
| Docker image | Multi-arch manifest for the server |
| SBOM files | CycloneDX format for each binary |
| Release notes | Human-readable change summary |

## Versioning

RMMWay follows semantic versioning:

- **Major** (1.0.0): Breaking API changes
- **Minor** (1.1.0): New features, backward-compatible
- **Patch** (1.0.1): Bug fixes, backward-compatible

## Post-Release

- Verify deployment of signed artifacts
- Announce the release (GitHub, mailing list, etc.)
- Update documentation links to the new version

---

*Release process documented as part of M5 integration milestone.*
