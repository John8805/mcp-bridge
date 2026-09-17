# mcp-bridge

Runs stdio MCP servers on this host and exposes each one over Streamable HTTP,
for an OpenConnector instance (usually in a container) that cannot spawn them
itself. OpenConnector writes the list of servers to a file; this bridge watches
that file, starts each server on first use, and answers `POST /<name>`.

Protocol details, the file format, and the two-token authentication are
documented in OpenConnector's `docs/mcp-bridge.md`.

## Build

```
go build -o mcp-bridge.exe .
```

## Run

```
mcp-bridge.exe -file C:\Users\john\Documents\open-connector\data\mcp-bridge.json -connector http://127.0.0.1:3010
```

| Flag         | Default                 | Meaning                                                              |
| ------------ | ----------------------- | -------------------------------------------------------------------- |
| `-file`      | (required)              | Bridge file written by OpenConnector.                                |
| `-listen`    | `0.0.0.0:7800`          | Address to serve on. Containers reach it via `host.docker.internal`. |
| `-connector` | `http://127.0.0.1:3010` | OpenConnector base URL, used to fetch a server's environment.        |
| `-idle`      | `30m`                   | Stop a server after this long without requests; `0` keeps them.      |
| `-poll`      | `1s`                    | How often the bridge file is checked for changes.                    |

`GET /health` lists the servers currently defined.

Set `OOMOL_CONNECT_MCP_BRIDGE_URL=http://host.docker.internal:7800` on the
OpenConnector side so it knows where to find this bridge.
