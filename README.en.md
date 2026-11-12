<div align="center">
  <h1>vnray</h1>
  <p>A V2Ray-based reverse proxy for NAT traversal and remote access.</p>
  <p>Expose the network access available to a client and centrally manage multiple egress clients.</p>
  <p>
    <a href="README.en.md"><strong>English</strong></a> ·
    <a href="README.md">简体中文</a>
  </p>
  <p>
    <a href="https://go.dev/"><img src="https://img.shields.io/github/go-mod/go-version/hhy50/vnray?style=flat-square" alt="Go version"></a>
  </p>
</div>

---

## What problem does it solve?

Many services run inside a home, office, or cloud private network:

- A home connection has no public IP, so a NAS, router, or development environment cannot be reached directly.
- Internal APIs, databases, or SSH services should not be exposed directly to the public Internet.
- Several machines can reach different networks, but users want one public entry point.
- The network only allows outbound connections, making inbound firewall or NAT rules inconvenient.

Typical private-network access solutions usually fall into two categories: **port mapping** and **network overlaying**.

- With port mapping, every client usually needs its own server-side port, and the setup is tightly coupled to the client configuration.
- With network overlaying, users and machines are placed into a virtual network, but the configuration and operational model can be complicated.

### The vnray approach

vnray takes a more focused approach: **one client exposes the network access available to that client**.

The server does not need to enter the private network. It only authenticates users, selects the target client, and forwards their traffic. The client then connects to the destination service from its own network and sends the response back through the tunnel.

Architecturally, this may look like installing a V2Ray server inside the client network and exposing it through a public port mapping. That is not how vnray works internally: the client maintains an outbound tunnel to vnray, and the public server routes user traffic through that tunnel.

## How it works

```text
┌────────┐        ┌──────────────┐        ┌──────────────┐        ┌──────────┐
│  User  │ ─────▶ │ Public server │ ◀───── │ Private client│ ─────▶ │ Target   │
└────────┘        │ Auth / route │  tunnel  │ Access network│        │ service  │
                  │ / management │        └──────────────┘        └──────────┘
                  └──────────────┘
```

| Component | Typical location | Main responsibility |
| --- | --- | --- |
| **Server** | A machine with a public entry point | Accepts user connections, maintains client sessions, routes requests according to bindings, and serves the admin console |
| **Client** | A private or target network | Connects to the server proactively and accesses services on the local machine or its surrounding network |
| **User client** | The user's device | Connects to the server with VLESS, VMess, or Shadowsocks |

## Core capabilities

- **Client-initiated tunnels** — The server does not need inbound access to the private network, which works well with NAT and outbound-only networks.
- **Multiple client egresses** — Connect several clients to one server and bind different users to different clients; `server:self` is reserved for local server egress.
- **VLESS / VMess / Shadowsocks access** — Connect with standard clients; mux and XUDP are supported.
- **Port mapping** — Expose a TCP service reachable by the client on a public port, with per-mapping upload/download traffic accounting.
- **Link encryption** — The tunnel between client and server defaults to ChaCha20-Poly1305 AEAD and can be turned off in the admin console.
- **User egress rules** — Reject/direct lists are enforced by the server and apply to all inbound protocols.
- **Routing and host mapping** — Client-side direct rules and domain-to-IP mappings, pushed hot from the admin console.
- **Bandwidth and concurrency controls** — Limit upload/download bandwidth and concurrent connections per user.
- **Admin console** — Manage users, plugins, keys, and port mappings online, with live logs and performance metrics in English and Chinese.
- **Persistent storage** — SQLite (default) or MySQL.

## Quick start

Download `vnserver` (server) and `vnclient` (client) for your platform from the [Releases](https://github.com/hhy50/vnray/releases) page, and grab the example configuration from the repository's [`configs/`](configs/) directory.

### 1. Start the server

The example [`configs/server.yaml`](configs/server.yaml) uses: `9000` for client tunnels, `8080` for the admin console (credentials `admin/admin`), and `bindings.db` as the SQLite database.

```bash
./vnserver -c configs/server.yaml
```

On first startup the database is initialized automatically and a bootstrap key `2nC7NBBcutpkJg0cPkyQgkzXqtWrri/JB4QyXI5N258=` with scope `default:*` is inserted (matching the example client configuration). Regenerate keys in the admin console for production use.

### 2. Start the client

On a machine that can reach the target service, edit [`configs/client.yaml`](configs/client.yaml) and start:

```yaml
server:
  addr: <public-server-address>:9000
  auth_key: 2nC7NBBcutpkJg0cPkyQgkzXqtWrri/JB4QyXI5N258=
group: default        # group; omitted means default
name: desktop-01      # instance name; omitted generates <hostname>-<IPv4>
```

```bash
./vnclient -c configs/client.yaml
```

The client maintains a long-lived connection and reconnects automatically after a disconnection.

### 3. Open the admin console and create a user

Open `http://<server-address>:8080`:

1. Confirm the client is online under **Clients**.
2. Enable the inbound protocol and set its listening port under **Plugins**.
3. Create a user under **Users**, pick the protocol and egress client, then copy the connection info into your V2Ray / Shadowsocks client.
4. To expose a TCP service in the client's network, create a mapping under **Port Mappings**.

For OpenWrt, [`openwrt/`](openwrt/README.md) provides a client package with a LuCI settings page, startup management, and multi-architecture packaging.

## Ports and protocols

| Purpose | Default / configuration | Description |
| --- | --- | --- |
| Client tunnel | `9000`, `server.listen` | The port clients connect to proactively; it does not carry user protocol traffic |
| vnray-user inbound | Set in the plugin page, e.g. `9001` | Separate port for `vn-v1` users; disabled until configured |
| Admin console | `8080`, `admin.listen` | Manage clients, users, plugins, and port mappings |
| VMess / VLESS / SS inbound | Configured in the admin console | For standard client connections |

## Configuration files

- [`configs/server.yaml`](configs/server.yaml): listener, authentication, admin console, storage, plugin, and logging settings.
- [`configs/client.yaml`](configs/client.yaml): server address, authentication key, reconnect policy, access controls, and routing.

```text
-c, --config  YAML configuration file; use built-in defaults when omitted
```

The client also supports temporarily overriding the server address with `--server <address>:9000`.

## Port mapping or protocol access?

- **Multiple users connecting with standard proxy clients**: create VLESS, VMess, or Shadowsocks users in the admin console.
- **Only one internal TCP service to expose**: use port mapping, for example to expose internal SSH on a public port.
- **Traffic must leave through a specific network**: bind the user to the corresponding client.

## Security

The example configuration is intended for local verification and contains public default admin credentials and a public bootstrap key. Before exposing the service to the public Internet:

- Change the admin username and password.
- Generate new client keys with per-client or per-group scopes and remove the bootstrap key.
- Open only the ports you actually need and restrict access to the admin console.
- Set reasonable bandwidth, concurrency, and client access limits for each user.
- For production use, prefer VLESS with TLS and protect certificates and private keys carefully.

## Documentation

- **Operations guide** (key management, encryption switch, client config push, log page, metrics, upgrade notes): [docs/GUIDE.md](docs/GUIDE.md) (Chinese)
- **Architecture decisions**: [docs/adr/](docs/adr/)

## Project status

vnray is under active development. Protocols, configuration, and admin console APIs may change. Admin configuration changes are not serialized across requests; concurrent saves may overwrite one another. Issues and improvement suggestions are welcome.
