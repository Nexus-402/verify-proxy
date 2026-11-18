# Security policy

This code checks that a request really comes from Nexus402 and was paid for.
A flaw in it could let someone call a creator's tool without paying, so we
want to hear about one before anyone else does.

## Report a vulnerability

Email **security@nexus402.dev**. Please do **not** open a public issue or pull
request for a security problem.

Include what you can of:

- which repository and version (`verify-proxy version`, or the commit of the
  starter kit),
- the steps to reproduce it, or a proof of concept,
- what an attacker could do with it.

Our full disclosure policy, including how we respond and how we treat
good-faith research, is at https://www.nexus402.dev/security/, and our
contact details are in https://www.nexus402.dev/.well-known/security.txt.

## Scope

In scope: the request verification (the Ed25519 signature, the creator key,
the 60-second window, the proof) and, in verify-proxy, how a verified call is
passed to the MCP server behind it and its answer returned.

Out of scope: a deployment's own configuration, such as an MCP server left
reachable from the internet beside the proxy, which lets callers go around it.

## Supported versions

verify-proxy: the latest release. Starter kits: the current `main`. Fixes are
made in Nexus402's own repository and arrive here with the next copy.
