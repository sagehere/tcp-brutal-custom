# Release signing trust

TCP Brutal Custom release manifests are signed with an offline Ed25519 key.

Public-key fingerprint (SHA-256 of DER SubjectPublicKeyInfo):

```text
b1a16baa2d9c68fdff5594e1261e0668f45b65253bf454b7c27025265b99bc1d
```

The private key is intentionally **not** stored in this repository or in GitHub Actions secrets.

Published releases must contain:

- `hashes.txt`
- `hashes.txt.sig`
- the release assets referenced by `hashes.txt`

Install/update verification first validates the Ed25519 signature over `hashes.txt`, then validates each downloaded artifact against that signed manifest.

For an already-installed system, the trusted public key is pinned locally and is not replaced by normal updates.

For a first installation, the public key still needs an independent trust bootstrap. Verify the fingerprint above through a channel you trust independently of the GitHub repository before granting root privileges to the installer.
