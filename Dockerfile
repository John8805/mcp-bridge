# The bridge plus the runtimes stdio MCP servers commonly need: node/npx for
# the npm ecosystem, uv/uvx and python3 for the Python one, git for packages
# fetched from repositories.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /mcp-bridge .

FROM node:24-alpine
RUN apk add --no-cache python3 git ca-certificates
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /usr/local/bin/
COPY --from=build /mcp-bridge /usr/local/bin/mcp-bridge
ENV MCP_BRIDGE_KEY=/data/mcp-bridge.key
VOLUME ["/data"]
EXPOSE 7800
ENTRYPOINT ["mcp-bridge"]
