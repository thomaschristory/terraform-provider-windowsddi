# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately via GitHub Security Advisories
(**Security, Report a vulnerability**) on this repository, rather than opening a
public issue. You will get an acknowledgement and a fix timeline.

## Hardening posture

- **Supply chain.** Every GitHub Actions `uses:` is pinned to a full commit SHA
  (version in a trailing comment). Dependabot keeps the SHAs and Go modules current.
- **Least privilege.** `test.yml` runs with `contents: read`. `release.yml` needs
  `contents: write` to create the GitHub release.
- **Release signing.** The GPG key that signs `SHA256SUMS` lives only in the
  repository secrets `GPG_PRIVATE_KEY` and `PASSPHRASE`. The release workflow runs
  only for `vX.Y.Z` tags and refuses tags that are not reachable from `main`.
  `main` is protected: required checks, linear history, no force pushes, enforced
  for admins.
- **Credentials at runtime.** The provider never logs passwords (redacted in
  `tflog` output) and never interpolates user values into PowerShell source; they
  are passed as a JSON parameters object (see [docs/DESIGN.md](docs/DESIGN.md)).
- **Repository settings.** Secret scanning with push protection, Dependabot alerts
  and security updates, CodeQL default setup and private vulnerability reporting
  are enabled.
