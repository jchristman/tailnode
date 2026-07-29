# tailnode

`tailnode` is a single-binary Go daemon (`package main`, flat layout at repo root) that joins a
Tailscale/Headscale tailnet via the embedded `tsnet` library and acts as a userspace subnet
router, forwarding TCP/UDP to hosts on advertised CIDRs. See `README.md` for full docs, flags,
and tuning.

## Cursor Cloud specific instructions

### Standard commands (see README.md "Build")
- Build: `go build -o tailnode .`
- Test: `go test ./...` (runs fully offline; no external services needed)
- Vet/format: `go vet ./...` and `gofmt -l .` (no dedicated linter config exists)
- The Go toolchain (1.26+) is preinstalled; `go mod download` is handled by the startup update script.

### Running the app end-to-end (non-obvious)
- The binary does nothing useful without a control server + auth key: `tsnet` must register with a
  Tailscale SaaS or self-hosted Headscale control plane. There is no default local listener.
- No auth-key/control-server secrets are provided in this environment. To exercise the product
  end-to-end without external accounts, run a local **Headscale** control server (downloaded
  ad hoc; not part of the repo) and point `tailnode` at it with `--login-server` + `--preauthkey`.
  Key Headscale config gotchas found during setup: set `unix_socket` to a writable path (default
  `/var/run/headscale` is not writable), and when `dns.override_local_dns` defaults on you must
  provide `dns.nameservers.global`.
- Advertised routes must be **approved** on the control side (`headscale nodes approve-routes`) or
  in the Tailscale admin console before traffic is forwarded.
- To route traffic through the node from a second peer, that peer must accept subnet routes
  (`RouteAll=true` / `--accept-routes`); otherwise it has no route to the advertised CIDR.
- Metrics are opt-in: pass `--metrics-addr 127.0.0.1:9090` to serve Prometheus text at `/metrics`.
- The node identity persists in `--state-dir`; deleting it re-registers a new node (new IP,
  unapproved routes). Pass `--state-dir` explicitly for reproducible runs.
