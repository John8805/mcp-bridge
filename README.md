# mcp-bridge

Runs stdio MCP servers on this host and exposes each one over Streamable HTTP,
for an OpenConnector instance (usually in a container) that cannot spawn them
itself. OpenConnector pushes this bridge its server list; the bridge keeps it
in memory, starts each server on first use, and answers `POST /<name>`.

The protocol, the payload, and how environments are encrypted are documented in
OpenConnector's `docs/mcp-bridge.md`.

## Build

```
go build -o mcp-bridge.exe .
```

Linux: `GOOS=linux GOARCH=amd64 go build -o mcp-bridge .`

## Run

```
mcp-bridge.exe -token <token you choose> -key mcp-bridge.key
```

On first start it generates a key pair, stores the private key in the `-key`
file, and prints the public key. Paste the token and that public key into the
`mcp_bridge` connection in OpenConnector, together with the URL the container
reaches this bridge at (`http://host.docker.internal:7800` on Docker Desktop).

| Flag      | Default          | Meaning                                                              |
| --------- | ---------------- | -------------------------------------------------------------------- |
| `-token`  | (required)       | Bridge token; the same value goes into the OpenConnector connection. |
| `-key`    | `mcp-bridge.key` | Private key file, created on first start. The only thing kept on disk. |
| `-listen` | `0.0.0.0:7800`   | Address to serve on.                                                 |
| `-idle`   | `30m`            | Stop a server after this long without requests; `0` keeps them.      |

Endpoints (all require `Authorization: Bearer <token>`):

- `PUT /config` — OpenConnector pushes the server list.
- `POST /<name>` — Streamable HTTP MCP endpoint of one server. Answers
  `503 {"error":"unconfigured"}` until a list has been pushed.
- `GET /health` — public key and the servers currently held.
