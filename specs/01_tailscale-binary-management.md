# Spec: Tailscale Binary Management

> **Status:** Implemented
> **Last Updated:** 2026-10-07

## Overview

The plugin image bundles the Tailscale binaries (`tailscale` and `tailscaled`) the plugin runs. The Dockerfile copies them from a pinned `tailscale/tailscale` image, which Dependabot updates. The plugin never downloads executables at runtime.

## Goals

- Every node running the same plugin version runs the same Tailscale version
- Starting a container does not depend on reaching a package server
- Air-gapped hosts need nothing beyond the plugin image

## Non-Goals

- Choosing a Tailscale version independently of the plugin version
- Running user-provided Tailscale binaries
- Managing multiple Tailscale versions simultaneously

## Configuration

None. To change the Tailscale version, install a plugin version that bundles it, or build the image with a different `tailscale/tailscale` tag.

Earlier versions downloaded Tailscale from pkgs.tailscale.com (`TS_VERSION=latest` or a version number) or used binaries from `TS_PATH`. These settings stay in the plugin configuration, so an upgraded installation keeps its settings and starts, but the plugin ignores them and logs a warning: a value of `TS_VERSION` other than `bundled`, and any `TS_PATH`.

## Behavior

The plugin runs `/usr/local/bin/tailscale` and `/usr/local/bin/tailscaled` from its image.

## Error Handling

| Condition | User Sees | Recovery |
|-----------|-----------|----------|
| Bundled binaries missing (broken image) | `bundled Tailscale binaries not found in /usr/local/bin` in the endpoint status; the plugin retries with backoff | Reinstall or rebuild the plugin image |
| `TS_VERSION` other than `bundled`, or `TS_PATH` set | Warning in the plugin log at startup | Unset the setting |

## Security Considerations

- **No runtime downloads** - The plugin fetches no executables, so a compromised or unreachable package server cannot affect running nodes
- **Pinned by digest** - The Dockerfile pins the `tailscale/tailscale` image by digest
