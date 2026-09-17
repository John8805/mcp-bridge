# mcp-bridge

Runs stdio MCP servers on a host and exposes each one over Streamable HTTP,
for an [OpenConnector](https://github.com/oomol-lab/open-connector) instance
(usually in a container) that cannot spawn them itself. OpenConnector pushes
this bridge its server list; the bridge keeps it in memory, starts each server
on first use, and answers `POST /<name>`.

Go, standard library only.

## Build

```
go build -o mcp-bridge.exe .
```

Linux: `GOOS=linux GOARCH=amd64 go build -o mcp-bridge .`

A `Dockerfile` builds an image that also carries `node`/`npx`, `uv`/`uvx`,
`python3` and `git`, for running a bridge next to OpenConnector in Compose.

## Run

```
mcp-bridge.exe -token <token you choose> -key mcp-bridge.key -connector-key <OpenConnector's signing key>
```

On first start it generates a key pair, stores the private key in the `-key`
file, and prints the public key. In OpenConnector, create an MCP Bridge
connection with the URL the container reaches this bridge at
(`http://host.docker.internal:7800` on Docker Desktop), the same token, and
that public key. OpenConnector's signing key is shown on its MCP page.

| Flag             | Default          | Env                        | Meaning                                                              |
| ---------------- | ---------------- | -------------------------- | -------------------------------------------------------------------- |
| `-token`         | (required)       | `MCP_BRIDGE_TOKEN`         | Bridge token; the same value goes into the OpenConnector connection. |
| `-connector-key` | (required)       | `MCP_BRIDGE_CONNECTOR_KEY` | OpenConnector's Ed25519 push-signing public key, base64.             |
| `-key`           | `mcp-bridge.key` | `MCP_BRIDGE_KEY`           | Private key file, created on first start. The only thing kept on disk. |
| `-listen`        | `0.0.0.0:7800`   |                            | Address to serve on.                                                 |
| `-idle`          | `30m`            |                            | Stop a server after this long without requests; `0` keeps them.      |

## Endpoints

All require `Authorization: Bearer <token>`.

- `PUT /config` — OpenConnector pushes the server list. The body must be
  signed (see below) or it is refused with `403 bad_signature`.
- `POST /<name>` — Streamable HTTP MCP endpoint of one server (protocol
  revision 2026-07-28: POST only, JSON responses, no sessions). Answers
  `503 {"error":"unconfigured"}` until a list has been pushed.
- `GET /health` — the bridge's public key and the servers currently held.

## Protocol

The pushed body:

```json
{
  "version": 1,
  "issuedAt": 1789640000000,
  "servers": {
    "seq": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-sequential-thinking"] },
    "fs":  { "command": "uvx", "args": ["mcp-server-fs", "/srv"], "cwd": "/srv",
             "env": { "ephemeralPublicKey": "BJ…", "nonce": "…", "ciphertext": "…" } }
  }
}
```

- **Signature.** The `X-Open-Connector-Signature` header carries a base64
  Ed25519 signature over the exact body bytes, verified against
  `-connector-key`. A push older than ten minutes by `issuedAt`, or older than
  the push already applied, is refused. A bridge token alone therefore lets a
  caller use the servers, but not hand the bridge commands to run.
- **Environment.** `env` is the server's environment encrypted to this bridge's
  P-256 key: ECDH with the ephemeral key, HKDF-SHA256 (empty salt, info
  `open-connector mcp bridge env v1`) to an AES-256-GCM key, 12-byte nonce,
  tag appended. The associated data is the entry's fields, each as a 4-byte
  big-endian length followed by its UTF-8 bytes, in the order name, command,
  cwd, then every argument — so a ciphertext only opens for the exact command
  it was encrypted for. It is decrypted in memory when the server is spawned
  and never written to disk.

## Behaviour

- One child process per server, started on the first request that needs it
  and kept for later requests. A crashed child is started again on the next
  request. Children run on their own console and process group.
- A pushed list that changes an entry stops its child so the next request
  starts it with the new definition; an entry that disappears stops its child.
- Requests to one server are serialized, since stdio carries one exchange at a
  time. `initialize` is answered from the handshake the bridge performed at
  spawn; server-initiated requests (sampling, elicitation) are refused.
- An interrupt stops every server and exits, logged.
