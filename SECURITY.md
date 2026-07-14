# Security Policy

## Reporting a vulnerability

Please report suspected security vulnerabilities privately rather than opening a
public issue. Use GitHub's **[Report a vulnerability](../../security/advisories/new)**
(Security → Advisories) so the report stays confidential until a fix is available.

Include, where possible:

- affected component (router, client, or shim) and version / commit,
- a description of the issue and its impact,
- steps to reproduce or a proof of concept.

We aim to acknowledge reports within a few days and to coordinate a fix and
disclosure timeline with you.

## Supported versions

Security fixes are applied to the latest released `major.minor` track. Older
tracks are not maintained.

## Deployment hardening

llmesh is self-hosted and its security depends on how it is deployed:

- **Terminate TLS** in front of the router (reverse proxy) and set
  `server.trust_proxy_headers: true` only when that proxy is trusted — this is
  what lets per-IP rate limiting and the session cookie's `Secure` flag work
  correctly. Leave it `false` when the router is directly exposed.
- **Keep API keys and client tokens secret.** They authenticate callers and
  workers respectively; anyone holding one can use the corresponding capability.
  The router stores only SHA-256 hashes of keys and tokens, so they cannot be
  recovered from a stolen state database — but they are shown exactly once at
  creation and travel in request headers, so protect them in transit and at
  the caller.
- **Restrict the local client API** (`local_api_addr`) to a loopback bind, or
  set `local_api_token`, since it serves unauthenticated inference otherwise.
- **Serve the update endpoint over HTTPS.** The client only auto-updates over
  TLS and only installs sha256-verified, strictly-newer binaries.
