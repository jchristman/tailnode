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

## Usage

```bash
go run . --advertise-route 192.168.0.0/24
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
| `--env-file` | `.env` | Path to env file |
| `--hostname` | `tailnode` | Hostname in the tailnet |
| `--state-dir` | | Directory for tsnet state |

After starting, approve the advertised routes in the [Tailscale admin console](https://login.tailscale.com/admin/machines) if required.

On Linux, enable IP forwarding for physical subnet access:

```bash
sudo sysctl -w net.ipv4.ip_forward=1
```

## Build

```bash
go build -o tailnode .
```
