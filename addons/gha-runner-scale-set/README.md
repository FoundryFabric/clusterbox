# gha-runner-scale-set

Stub for the GitHub Actions Runner Controller (scale sets) addon.

This directory currently contains only the addon manifest skeleton so the
catalog loader can discover it. The actual Helm chart reference and any
supporting Kubernetes manifests will land in T6.

## Required secrets

| Key                      | Description                  |
| ------------------------ | ---------------------------- |
| `GH_APP_ID`              | GitHub App ID                |
| `GH_APP_INSTALLATION_ID` | GitHub App Installation ID   |
| `GH_APP_PRIVATE_KEY`     | PEM-encoded private key      |

## Strategy

`helmchart` — the installer (T3) will render a single `manifests/helmchart.yaml`
once it is added.
