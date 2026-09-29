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
- **Guard the sign-in secrets in the state database.** OAuth client secrets
  (GitHub, Google, OpenID Connect) and an SMTP password, if you configure those sign-in
  methods, are stored in the settings table in plaintext — the router has no
  key to encrypt them under that it would not also store beside them. None is
  ever rendered back into the portal or written to the log, but anyone who can
  read the database file can read them. Clear any of them from
  **Settings → Sign-in** when rotating or decommissioning the credential.
- **Serve the portal over TLS before enabling email or OAuth sign-in.** A
  sign-in link is a bearer credential in transit; it lasts 15 minutes, works
  once, and is invalidated by requesting another, but over plain HTTP it is
  readable by anything on the path. The same applies to an OAuth callback,
  which carries an authorization code, and Google will not accept a plain-HTTP
  redirect URI except on localhost.
- **Provider roles take effect at sign-in, not instantly.** With OpenID
  Connect access control on, removing a user's role at the provider stops
  their next sign-in, but not a portal session already open or their API
  keys. Disable the user here as well when access must end now.
- **Access rules that match on source address trust the socket peer** unless
  `trust_proxy_headers` is on. Turn it on only behind a proxy that sets
  `X-Forwarded-For` itself; otherwise any caller could claim any address.
- **Register the redirect URI exactly.** Each provider's callback URL is shown
  on the settings page. Registering a broader pattern than the one shown, where
  a provider permits it, widens where an authorization code can be delivered.
- **Portal sessions are stored hashed** in the state database and survive a
  restart. Changing a password signs out the account's other sessions;
  resetting one or disabling the account signs out all of them.
- **Give API keys an expiry** where you can. An expired key is refused, and
  each key shows when it was last used.
- **Disable a user to cut off their access.** Disabling ends their portal
  session, refuses their API keys and client tokens, and disconnects any
  client already connected with one. Re-enabling restores them unchanged;
  deleting the user (only possible once disabled) destroys them.
- **Restrict the local client API** (`local_api_addr`) to a loopback bind, or
  set `local_api_token`, since it serves unauthenticated inference otherwise.
  It refuses browser requests, so a web page open on the machine cannot reach
  it through cross-site requests or DNS rebinding.
- **Clients never update themselves.** A router cannot change the code running
  on a client machine; upgrade a client by pulling a new image or replacing the
  binary yourself.
- **Containers run as non-root** (uid 10001) for the router, client, and shim
  images. Keep it that way — for bind-mounted state, chown the host directory
  to 10001 (rootful docker) or map your user with
  `--userns=keep-id:uid=10001,gid=10001` (rootless podman).
