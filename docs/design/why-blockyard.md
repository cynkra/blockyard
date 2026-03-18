# Why Blockyard?

Blockr apps execute arbitrary R code, manage per-user credentials, and persist
shared state. Hosting them on the public internet demands a security posture
that no existing Shiny platform was designed to provide. Blockyard is
purpose-built for this threat model.

## Isolation is non-negotiable

Every blockr session evaluates user-supplied R expressions. In a public-facing
deployment, each session must be treated as potentially hostile — isolated from
other sessions, from the host, and from the management plane. Blockyard
enforces this with per-container bridge networks, dropped capabilities,
read-only root filesystems, and seccomp profiles. Running containers under an
alternative runtime like Kata brings this close to bullet-proof: each session
gets its own lightweight VM, eliminating shared-kernel attack surface entirely.

Posit Connect was designed for trusted internal teams, not internet-facing
adversaries. Its namespace sandboxing is a different class of isolation than
what this threat model requires.

## Per-user credentials cannot use environment variables

Blockr apps connect to databases, APIs, and cloud services using credentials
that belong to the *user*, not the app. Any platform that delivers secrets via
environment variables is fundamentally broken here — `Sys.getenv()` is one line
of R code, and users can run arbitrary expressions. This rules out ShinyProxy
and Connect's standard credential model.

Blockyard integrates with OpenBao (Vault-compatible) to inject short-lived,
scoped tokens per request. The server itself cannot read user secrets. The R
process exchanges the token directly with OpenBao. No existing Shiny platform offers per-user credential isolation with
this trust model.

## One stack, ready to boot

A single `docker-compose up` gives you the complete blockr hosting stack:
hardened container isolation, per-user credential management via OpenBao, board
storage with ACLs — all wired together and configured correctly out of the box.

Assembling the equivalent from Connect + external Vault + custom board storage +
a reverse proxy is a significant integration project with no guarantee the
pieces fit together securely. Blockyard ships the full stack as one unit, with the security model
baked in.

## Building our own unlocks blockr-specific features

Owning the platform lets us build what blockr actually needs instead of working
around what a general-purpose host happens to offer:

- **Board storage** — first-class save/share/restore workflows with per-user
  ACLs, not bolted on after the fact
- **Live package installs** — users install packages at runtime instead of
  waiting for an image rebuild (ShinyProxy) or a full redeployment (Connect)

## What about the alternatives?

**Shiny Server** and **faucet** are process managers, not hosting platforms.
Neither provides containers, authentication, deployment pipelines, or credential
management. They solve a different, smaller problem.

**Scaly** and **ricochet** are closed-source platforms with limited public
documentation. You cannot audit their isolation model, extend their credential
handling, or verify their security posture. For a system whose core requirement
is provable isolation, that's a non-starter.
