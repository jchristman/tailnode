# tailnode

A small Go utility that joins a [Tailscale](https://tailscale.com) tailnet with [tsnet](https://pkg.go.dev/tailscale.com/tsnet) and advertises subnet routes. It forwards TCP and UDP traffic to hosts on those subnets so other tailnet devices can reach LAN resources without running the full Tailscale client.

## Requirements

- Go 1.26+
- A Tailscale auth key with subnet routing enabled

## Setup

Create a `.env` file with your auth key:

```
AUTH_KEY=tskey-auth-...
```

Or set `AUTH_KEY` / `TS_AUTHKEY` in the environment, or pass `--preauthkey`.

For [Headscale](https://headscale.net) or another control server, set the login URL:

```
LOGIN_SERVER=https://headscale.example.com
```

Or pass `--login-server`, or set `CONTROL_URL` / `TS_CONTROL_URL`.

## Usage

```bash
go run . --advertise-route 192.168.0.0/24
```

```bash
go run . --advertise-route 192.168.0.0/24 --login-server https://headscale.example.com
```

Multiple routes can be comma-separated:

```bash
go run . --advertise-route 192.168.0.0/24,10.0.0.0/8
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--advertise-route` | *(required)* | Subnet CIDR(s) to advertise |
| `--preauthkey` | | Auth key (overrides `.env`) |
| `--login-server` | | Control server URL (Headscale/custom) |
| `--env-file` | `.env` | Path to env file |
| `--hostname` | `tailnode` | Hostname in the tailnet |
| `--state-dir` | | Directory for tsnet state |
| `--metrics-addr` | | Serve metrics on this address (empty disables) |
| `--verbose` | `false` | Log tsnet and netstack internals |

After starting, approve the advertised routes in the [Tailscale admin console](https://login.tailscale.com/admin/machines) if required.

On Linux, enable IP forwarding for physical subnet access:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
```

## Tuning for high volume

### File descriptors

Every proxied TCP flow and every UDP flow holds one host descriptor, so
`RLIMIT_NOFILE` is the hard ceiling on concurrency. tailnode raises its own soft
limit toward the hard limit at startup and logs what it ended up with:

```
limits: fds soft=65535 hard=65535, max concurrent dials=512, max proxied conns=4096, dial slot wait=100ms
```

A soft limit under 4096 is logged as a warning. The default of 1024 on many
systems caps the node near a thousand flows, which shows up as backend dial
failures rather than an obvious limit. Use the provided
[systemd unit](deploy/tailnode.service), which sets `LimitNOFILE=65535`.

### Concurrency flags

| Flag | Default | Description |
|------|---------|-------------|
| `--max-concurrent-dials` | auto | Parallel backend dials; `-1` derives from the descriptor budget, `0` is unlimited |
| `--max-proxied-conns` | auto | Concurrent proxied flows; `-1` derives from the descriptor budget, `0` is unlimited |
| `--dial-slot-wait` | `100ms` | How long a new flow waits for a dial slot before being reset |
| `--backend-dial-timeout` | `500ms` | Timeout for backend TCP dials |
| `--idle-timeout` | `0` | Close proxied TCP flows idle this long (`0` leaves it to netstack keepalives) |

The dial slot wait is deliberately short. Netstack invokes our accept callback
while holding a gVisor forwarder slot and a per-client in-flight slot, and once
the forwarder table (8192 entries) fills, gVisor drops further SYNs *without* a
RST. Clients read those silent drops as rate limiting, so it is better to reset
a flow quickly than to stall on the accept path.

### Reachability cache

Repeat traffic to a known backend skips the dial on netstack's accept path
entirely, which is what allows the node to use the full forwarder width.

| Flag | Default | Description |
|------|---------|-------------|
| `--reachability-cache` | `true` | Enable the cache; `false` restores strict dial-per-flow |
| `--reachability-ttl` | `10s` | How long a reachable backend stays cached |
| `--reachability-negative-ttl` | `3s` | How long an unreachable backend stays cached |
| `--reachability-cache-size` | `65536` | Max cached entries |

Tradeoff: within the positive TTL, a port that has just closed will complete the
client handshake and then reset, instead of refusing the connection outright.
The negative TTL is kept short so a service that comes up is not blackholed. A
first sweep over many unique ports still dials once per port; the cache only
helps traffic that repeats.

Only `ECONNREFUSED` is cached as unreachable, because only an RST comes from the
target's own stack. Timeouts are ambiguous, and `EHOSTUNREACH`/`ENETUNREACH` are
raised locally by ARP or by a router's ICMP unreachable — measured against a
filtering host under churn, they landed on a few percent of dials to ports that
were merely filtered. Caching any of those would report a filtered port as
closed, so they are re-dialed on the next flow instead.

### Known limitation: filtered ports are reported as closed

When a backend dial fails, the flow is reset, and a client turns that RST into
"connection refused". That is accurate when the target refused, but a dial also
fails when the target is *filtering* the port, or when this node hits its own
dial-slot or connection ceiling. In all of those cases the honest answer is to
send nothing and let the client time out, which is what makes a scanner report
"filtered" rather than "closed".

netstack cannot express that. Every path through its `acceptTCP` ends in either
`Complete(true)`, which resets, or a created endpoint, which completes the
handshake and makes the port look open; `GetTCPHandlerForFlow` has no way to ask
for a silent drop. `classifyFlow` therefore decides the correct verdict —
`verdictReset` only on a refusal, `verdictDrop` for everything ambiguous — and
`tcpHandlerForFlow` downgrades a drop to a reset, counting it:

| Counter | Meaning |
|---------|---------|
| `tailnode_tcp_reset_refused` | Reset because the target refused. The client's "closed" is accurate. |
| `tailnode_tcp_reset_ambiguous` | Reset where a drop was wanted. The client reports "closed" for a port that was filtered, or for one this node lacked capacity to reach. |

Measured against a host that drops on closed ports, an `nmap -sT` of 1024 ports
came back as 1022 "closed (conn-refused)" when every one of those ports was in
fact filtered. Treat `tailnode_tcp_reset_ambiguous` as the count of scan results
that cannot be trusted to distinguish closed from filtered. Lifting this needs a
drop verdict in tailscale's netstack hook.

The practical consequence is that a **filtering** target gets no benefit from the
cache: every flow pays a full `--backend-dial-timeout` while holding a dial slot,
so the ceiling is roughly `--max-concurrent-dials / --backend-dial-timeout`
(about 1000 flows/sec at the defaults of 512 and 500ms). If you are sweeping a
host that drops rather than refuses, raise `--max-concurrent-dials` or lower
`--backend-dial-timeout` — watch `tailnode_tcp_dial_slot_timeout` to see whether
that ceiling is actually binding.

### UDP

| Flag | Default | Description |
|------|---------|-------------|
| `--udp-preserve-source-port` | `false` | Bind backend UDP sockets to the client's source port |

Source-port preservation needs privileges for ports below 1024 and collides when
two clients share a source port, so backend sockets use an ephemeral port by
default. Backend replies from an address other than the intended destination are
dropped.

### Path MTU

The tunnel defaults to a conservative 1280-byte MTU, which limits single-flow
bandwidth on long-RTT paths. `TS_DEBUG_ENABLE_PMTUD=1` enables path MTU
discovery.

## Metrics

`--metrics-addr 127.0.0.1:9090` serves Prometheus text at `/metrics` and the
full expvar tree, including netstack's own counters, at `/debug/vars`.

The counters that matter when throughput looks like rate limiting:

- `tailnode_tcp_dial_fail_timeout` vs `tailnode_tcp_dial_fail_refused` separates
  a saturated path from a closed port.
- `tailnode_tcp_dial_slot_timeout` and `tailnode_tcp_conn_limit_refused` mean
  tailnode's own ceilings are binding; raise the descriptor limit or the flags.
- `tailnode_tcp_handoff_reaped` counts backends closed because the client never
  completed its handshake, which is normal during half-open (`nmap -sS`) scans.
- `tailnode_reachability_hit` / `_miss` shows whether traffic is benefiting from
  the cache.
- From `/debug/vars`, `netstack.counter_tcp_forward_max_in_flight_drop` and
  `netstack.counter_tcp_forward_max_in_flight_per_client_drop` count SYNs that
  netstack discarded before reaching tailnode. Anything above zero there is the
  silent-drop path that clients misread as throttling.

## Deploy

Either unit sets `Restart=always` and raises the descriptor limit. Use the
[system unit](deploy/tailnode.service) to run as root:

```bash
sudo cp deploy/tailnode.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tailnode
```

Or the [user unit](deploy/tailnode.user.service) to run unprivileged. Enable
lingering so it survives logout and starts at boot:

```bash
mkdir -p ~/bin ~/.config/systemd/user
install -m 0755 tailnode ~/bin/tailnode
cp deploy/tailnode.user.service ~/.config/systemd/user/tailnode.service
sudo loginctl enable-linger "$USER"
systemctl --user daemon-reload
systemctl --user enable --now tailnode
```

Install the binary to a stable path rather than overwriting it in place; the
unit runs `~/bin/tailnode`, so a new build can be staged elsewhere and moved in.

### Keeping the same node across restarts

The node's identity lives in the state directory. Lose it and tsnet registers a
new node with the auth key, which gets a new tailnet IP and whose subnet routes
are **unapproved**, so the route has to be approved again in the admin console.

tsnet's own default for that directory is derived from `filepath.Base(os.Args[0])`,
so running the same build as `tailnode.new` or `tailnode-linux-amd64` silently
points it at an empty directory and creates a second node. This build ignores
the binary name and defaults to `<user config dir>/tsnet-tailnode`; the units
pass `--state-dir` explicitly as well. Startup logs the path it settled on:

```
tsnet state: /home/you/.config/tsnet-tailnode
```

Back that directory up before upgrading, and make sure only one process uses it
at a time. `Ignoring authkey` in the log is the good case: it means the existing
node key was reused rather than a fresh registration.

Two settings on the tailnet side finish the job. Approve the route once from
the admin console, or auto-approve it so a re-registered node needs no manual
step, by adding to the tailnet policy file:

```json
"autoApprovers": {
    "routes": {
        "192.168.0.0/24": ["your-login@example.com"]
    }
}
```

Then disable key expiry for the node, otherwise it drops off the tailnet when
the node key expires and has to re-authenticate.

## Build

```bash
go build -o tailnode .
```
