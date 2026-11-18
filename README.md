<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/nexus402-logo-for-dark-bg.png">
    <img alt="Nexus402" src="assets/nexus402-logo-for-light-bg.png" width="360">
  </picture>
</p>

# verify-proxy

Put an MCP server you already run on Nexus402 **without changing its code**.

`verify-proxy` runs beside your server and becomes the public HTTPS endpoint
you publish. It lets through only calls that Nexus402 signed and a developer
already paid for, passes each one to your server with the arguments
untouched, and returns your server's answer with the proof Nexus402 needs.

```
Nexus402 ──signed, paid call──▶ verify-proxy ──plain MCP──▶ your MCP server
         ◀──result + proof────  (public HTTPS)  ◀──answer───  (private address)
```

It works with all three ways MCP servers talk:

| Your server speaks | Run |
|---|---|
| Streamable HTTP (current MCP) | `UPSTREAM_URL=http://127.0.0.1:3000/mcp verify-proxy` |
| HTTP + SSE (older MCP) | the same; the proxy detects it |
| stdio (a local process) | `verify-proxy -- python my_server.py` |

## Run it

Download a binary for your platform from the
[releases](https://github.com/Nexus-402/verify-proxy/releases), or build it
with Go 1.22 or later (standard library only):

```bash
go build -o verify-proxy .
```

Then set the same variables every Nexus402 starter kit uses:

```bash
export PORT=8080
export NEXUS_REGISTERED_HOST=mcp.you.example   # the host in the URL you publish
export NEXUS_CREATOR_KEY=nx_crt_...             # shown once when you claim your handle
export NEXUS_PUBLIC_KEY=<base64>                # GET https://api.nexus402.dev/v1/signing-key

# an HTTP server:
export UPSTREAM_URL=http://127.0.0.1:3000/mcp
./verify-proxy

# or a stdio server, which the proxy starts and restarts for you:
./verify-proxy -- node build/index.js
```

Put it behind HTTPS on port 443 (a load balancer, a reverse proxy, or your
host's TLS), then publish.

### With Docker

```bash
docker run -p 8080:8080 \
  -e UPSTREAM_URL=http://my-mcp-server:3000/mcp \
  -e NEXUS_REGISTERED_HOST=mcp.you.example \
  -e NEXUS_CREATOR_KEY=nx_crt_... -e NEXUS_PUBLIC_KEY=... \
  ghcr.io/nexus-402/verify-proxy:latest
```

For a stdio server, the proxy and the server must run in one container:
build your server's image and copy the binary in.

```dockerfile
FROM your-mcp-server-image
COPY --from=ghcr.io/nexus-402/verify-proxy:latest /verify-proxy /usr/local/bin/verify-proxy
ENTRYPOINT ["verify-proxy", "--", "python", "server.py"]
```

## Publish

1. **List your tools** in the form the publish page wants:

   ```bash
   UPSTREAM_URL=http://127.0.0.1:3000/mcp ./verify-proxy tools
   # or: ./verify-proxy tools -- python my_server.py
   ```

   It prints each tool's `name`, `description` and `input_schema`, ready to
   paste. Publish each tool under your handle, for example
   `you/weather/get_forecast`, with your proxy's URL as the endpoint. The
   proxy turns `you/weather/get_forecast` back into `get_forecast` for your
   server.

2. **The first time at a new host**, the publish answers
   `host_proof_required` with a `challenge_url` and a `challenge_content`.
   Give both to the proxy and publish again; it serves the file for you:

   ```bash
   export NEXUS_CHALLENGE_URL=https://mcp.you.example/.well-known/nexus402-challenge/...
   export NEXUS_CHALLENGE_CONTENT=nexus402-host-proof-v1:...
   ```

   Once a version is live at that host you can remove them.

## Keep your server private

**Your MCP server must be reachable only by the proxy**: listen on
`127.0.0.1`, a private network, or a container network, never the internet.
If your server is public, anyone can call it directly, skipping the proxy
and payment. The proxy cannot prevent that; only where you bind your server
can.

## What a caller pays for

**A developer never pays for your tool's failure.** They pay when your tool
did its work, or when their own input made it fail after it did.

| What happens | The proxy returns | The developer |
|---|---|---|
| your server answers with a result | `200`, the result exactly as written | pays |
| the arguments do not fit the tool's input schema | `400`, **without calling your server** | is refunded |
| a tool your server does not list in `tools/list` | `404`, without calling your server | is refunded |
| your server answers `isError: true` (your tool failed: an API down, a crash) | `502`, with your tool's message | is refunded; counts against your endpoint's health |
| your server answers `isError: true` **and** `"_meta": {"nexus402/fault": "client"}` (the caller's input made it fail after your tool did work) | `200`, the result | pays |
| JSON-RPC error `-32602` / `-32600` / `-32700` (bad request) | `400` | is refunded |
| JSON-RPC error `-32601` (no such method) | `404` | is refunded |
| any other JSON-RPC error | `502` | is refunded; counts against your endpoint's health |
| nothing in 120 seconds, or it is down | `504` / `502` | is refunded; counts against your endpoint's health |

The schema check uses the `inputSchema` your server lists. It checks types,
required fields, allowed values, numeric, length and item limits, and fields
a schema with `additionalProperties: false` does not name. A part of a schema
it does not understand (`anyOf`, `$ref`, ...) is left for your server to
judge, so the check never refuses a call your server would accept.

To charge for a failure the caller caused after your tool did work, add the
mark to that result:

```json
{"content":[{"type":"text","text":"No forecast for that date."}],
 "isError":true, "_meta":{"nexus402/fault":"client"}}
```

## What it does on every request

1. Refuses anything not signed by Nexus402, older than 60 seconds, replayed
   to another host or path, changed in transit, or without your creator key
   (`401`, and your server never sees it). The check is the same code as the
   Go starter kit's `verify.go`.
2. Answers the publish-time hello and the host proof itself.
3. Checks the arguments against the tool's input schema, then changes the
   tool name to your server's own, and nothing else: the arguments reach
   your server byte for byte.
4. Speaks MCP to your server: initializes once, keeps the session (and
   starts a new one if your server forgets it), reads JSON or event-stream
   answers, answers your server's `ping`, and cancels a request upstream
   when the caller goes away.
5. Returns only the `result`. Nexus402 adds the JSON-RPC envelope, so passing
   your server's on would wrap it twice.

It logs refusals and failures by kind, and never a request's or an answer's
content. It does not meter, hold any developer's key, or call anything but
your own server.

## How it is kept correct

This repository is a read-only copy of code developed in Nexus402's own
repository. There, the proxy is built from exactly these files and put in
front of four MCP servers that know nothing of Nexus402 (Streamable HTTP with
JSON answers, Streamable HTTP with sessions and event streams, HTTP + SSE,
and stdio). Each must pass the starter kits' contract, the gateway's real
hello, a real signed call and seven forged requests, plus byte-exact
arguments, the schema check, who pays for a failure, error mapping, a
forgotten session and concurrent calls. Each release is also run against
the official MCP reference server. Only a
copy that passed is published here.

Issues and pull requests are welcome; an accepted change arrives here with
the next copy. The contract: https://www.nexus402.dev/docs/creators/requirements/

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
