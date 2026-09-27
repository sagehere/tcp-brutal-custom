# Release signing trust

TCP Brutal Custom release manifests are signed with an offline Ed25519 key.

Public-key fingerprint (SHA-256 of DER SubjectPublicKeyInfo):

```text
249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff
```

The private key is intentionally **not** stored in this repository or in GitHub Actions secrets.

Published releases must contain:

- `hashes.txt`
- `hashes.txt.sig`
- the release assets referenced by `hashes.txt`

Install/update verification first validates the Ed25519 signature over `hashes.txt`, then validates each downloaded artifact against that signed manifest.

For an already-installed system, the trusted public key is pinned locally and is not replaced by normal updates.

For a first installation, the public key still needs an independent trust bootstrap. Verify the fingerprint above through a channel you trust independently of the GitHub repository before granting root privileges to the installer.


## One-command bootstrap

`scripts/bootstrap.sh` provides the convenient one-command installation path. It embeds the same release public key and fingerprint, verifies the signed manifest and the installer checksum, and only then executes `install.sh`.

This improves usability without weakening Release verification, but it does **not** remove the first-install bootstrap problem: if an attacker already controls the GitHub repository before a user obtains `bootstrap.sh`, the attacker can change both the script and documentation. High-assurance first installs therefore still require checking the fingerprint through an independent trusted channel.

## Migration from unsigned versions

Systems installed before signed-manifest verification was introduced still have an older updater that trusts GitHub Release plus SHA-256 only. Their **first** move to a signed release cannot be retroactively protected by the new verifier.

For security-sensitive hosts, perform that migration manually: independently verify the public-key fingerprint, verify the signed release manifest and installer as described in the README, and then run the verified installer. After that migration, subsequent updates use the locally pinned key and fail closed on missing or invalid signatures.


## Key rotation on 2026-09-28

The original offline Ed25519 private key was lost. Its public-key fingerprint was:

```text
b1a16baa2d9c68fdff5594e1261e0668f45b65253bf454b7c27025265b99bc1d
```

It has been replaced with a new offline Ed25519 key whose fingerprint is:

```text
249a5abded1a497f8fe67f4cf5cd8e47d127b9cee2d9b1eac23042b7edfeceff
```

Because the old private key is unavailable, the rotation cannot be cryptographically authorized by the old key. Existing installations that pinned the old key must perform an explicit administrator-approved trust migration before they can accept releases signed by the new key.

Use `scripts/migrate-release-key.sh` only after independently verifying the new fingerprint above.
