# Clusterbox — Architecture Directives

## Node install is always clusterboxnode, never raw SSH scripts

All logic that runs on a remote node must live inside `clusterboxnode`
(`cmd/clusterboxnode/`, `internal/node/`). The provider layer may SSH into a
node to upload and invoke the binary, but it must not run raw shell commands as
a substitute for programmatic logic.

**What belongs in clusterboxnode:**
- k3s install/uninstall
- Waiting for a service to become healthy (e.g. `waitForAgent`)
- Post-failure diagnostics (e.g. `collectAgentDiagnostics`)
- Any node-level configuration (hardening, tailscale, etc.)

**What belongs in the provider:**
- SSH transport (upload binary, invoke it, stream stdout back)
- Interpreting the JSON result envelope
- Cloud/VM lifecycle (create server, port-forward, destroy)

The SSH utility functions (`RunNodeAgent`, `CollectAgentDiagnostics`,
`WaitForSSH`, etc.) live in `internal/provision/nodeinstall` and are shared by
all SSH-based providers (QEMU, Hetzner). Do not duplicate them per-provider.

## Diagnostics run inside clusterboxnode, not via SSH from the provider

When k3s-agent fails to join a cluster, diagnostics (`journalctl`, `systemctl
status`, `ip addr`, `curl`) are collected inside `internal/node/k3s` using the
`Runner` interface. clusterboxnode runs as root (via sudo), so no privilege
escalation is needed. Output streams back through the existing SSH session.

Do not add SSH-based diagnostic commands in provider code as a primary path.
`CollectAgentDiagnostics` in `nodeinstall` is a fallback for cases where
clusterboxnode itself cannot start (SSH transport failure).

## Provider interface is cloud-agnostic

`provision.Provider` must not leak provider-specific concepts. Port forwarding,
QCOW2 disks, PID files, hcloud IDs, Tailscale devices — all stay inside their
respective provider implementations. The interface surface is:
`Provision`, `Destroy`, `Reconcile`, `AddNode`, `RemoveNode`.

## QEMU-specific decisions

- Workers join via `https://10.0.2.2:<cpK3sPort>` — SLIRP routes this to the
  CP VM's port 6443. **Do not use the multicast network (net1) for join traffic
  — it does not work on macOS.**
- CP k3s cert must include `TLSSANs: ["127.0.0.1", "10.0.2.2"]`.
- `Provision` deletes `disk.qcow2` before recreating it so k3s is always
  freshly installed with current TLS SANs. Never skip this step.
- SSH port for each worker is pre-selected inside the mutex (via `pickFreePort`
  OS-assigned random port) during index allocation. Do not use sequential port
  scanning for concurrent workers.
- QEMU port-bind failures are caught 600 ms after launch via
  `checkQEMULogForErrors`. Do not skip this check.

## Testing conventions

- `internal/node/k3s`: uses `Runner` and `FS` interfaces — all tests are pure
  unit tests with no real processes or filesystem access.
- `fakeRunner.runResp` keys on `"name arg0 arg1..."` (full command) with
  fallback to `"name"` only. Use specific keys when the same binary is called
  with different args (e.g. `"systemctl is-active k3s"` vs
  `"systemctl is-active k3s-agent"`).
- Provider tests inject `Deps.Bootstrap` / `Deps.Runner` stubs. Do not spawn
  real QEMU or make real hcloud API calls in unit tests.
