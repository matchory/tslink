# Spec: Docker Plugin Multi-Architecture Builds

> **Status:** Implemented
> **Last Updated:** 2026-10-07

## Overview

The plugin is built for multiple CPU architectures (amd64, arm64) and published to GHCR. Due to Docker plugin registry limitations, each architecture is published as a separate tag rather than a unified multi-arch manifest.

## Goals

- Support both amd64 and arm64 architectures
- Publish to GHCR on every push to main and on version tags
- Clear installation instructions for each architecture

## Non-Goals

- Unified multi-arch manifest (not supported by Docker plugins)
- Support for additional architectures (386, ppc64le, etc.)
- Automatic architecture detection during install

## Background: Docker Plugin Registry Format

Docker plugins use a different registry format than standard OCI container images:

| Aspect | Container Images | Docker Plugins |
|--------|------------------|----------------|
| Build command | `docker build` | `docker plugin create` |
| Push command | `docker push` | `docker plugin push` |
| Manifest support | Yes (OCI Image Index) | No |
| Platform metadata | Embedded in manifest | Not present |

When attempting to create a manifest list for Docker plugins:

```
docker manifest create ghcr.io/org/plugin:latest \
  ghcr.io/org/plugin:v1-amd64 \
  ghcr.io/org/plugin:v1-arm64
```

The operation fails:

```
manifest ghcr.io/org/plugin:v1-amd64 must have an OS and Architecture
```

This occurs because `docker plugin push` does not include platform annotations in the registry blob, which `docker manifest` requires.

## Solution

Publish each architecture as a separate tag:

| Trigger | Tags Published |
| --- | --- |
| Push to `main` | `:main-amd64`, `:main-arm64` |
| Push tag `v0.0.1` | `:v0.0.1-amd64`, `:v0.0.1-arm64`, `:latest-amd64`, `:latest-arm64`, plus a GitHub release |

Pull requests publish nothing; the release workflow runs only on pushes to `main` and on `vX.Y.Z` tags.

Users specify their architecture explicitly:

```bash
# amd64 (Intel/AMD, most cloud VMs)
docker plugin install ghcr.io/matchory/tslink:latest-amd64

# arm64 (Apple Silicon, AWS Graviton)
docker plugin install ghcr.io/matchory/tslink:latest-arm64
```

## Build Pipeline

1. Matrix build runs for each architecture in parallel
2. Native runners for each architecture (`ubuntu-24.04` for amd64, `ubuntu-24.04-arm` for arm64)
3. Each job builds the plugin rootfs from `docker/Dockerfile`, creates the plugin and pushes its architecture-specific
   tag(s)
4. On a version tag, the `latest-<arch>` plugin gets a `.plugin-version` marker in its rootfs: Docker refuses to
   create a second plugin from identical content
5. After both architectures are pushed, a version tag creates a GitHub release with generated notes
6. No manifest job required

Both architectures build in ~1-2 minutes using native runners.

## Security Considerations

- **No PR publishing**: Pull requests, including those from forks, never get GHCR write access
- **Version tags**: `:v0.0.1-*` are pushed once per version tag (re-running the workflow would push them again)
- **Mutable development tags**: `:main-*` updated on each push to main
- **Latest tracks releases**: `:latest-*` only updated on version tags, not main pushes

## Alternatives Considered

| Approach | Rejected Because |
|----------|------------------|
| OCI image wrapping plugin rootfs | Breaks standard `docker plugin install` workflow |
| Single architecture only | arm64 increasingly common (Apple Silicon, Graviton) |
| Separate repositories per arch | Confusing for users, harder to maintain |

## Future Enhancements

1. **Architecture detection script** - Helper script that detects arch and installs correct tag
2. **OCI plugin format** - If Docker adds manifest support for plugins
