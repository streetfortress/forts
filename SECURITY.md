# Security Policy

Forts generates, encrypts, and distributes machine identities and secrets.
If you find a weakness in how it does that, we want to hear from you
privately first.

## Reporting a vulnerability

Email **security@streetfortress.com**. Include the version or commit, the
verb or module involved, and steps to reproduce. We will acknowledge your
report and keep you informed as we work on it. Please give us a reasonable
window to ship a fix before you disclose publicly.

Do not open a public issue for a vulnerability.

## Scope

Reports we care most about:

- Secrets handling: sops encryption, key custody in `fort mint` and
  `fort seed`, anything that could land a private key or passphrase in the
  repo, the image, or command output in plaintext.
- The mesh: WireGuard key handling, peer verification in `fort link` /
  `fort estate link`.
- Provisioning: the image pipeline and the host-key seed channel.
- The hardened host profile under `modules/`.

Out of scope: vulnerabilities in NixOS, Flux, k3s, pulumi, or other
upstream components (report those upstream), and misconfiguration of an
estate you operate.

## Supported versions

The latest tagged release. Fixes land on `main` and ship in the next tag.
