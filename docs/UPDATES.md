# Signed updates

`dacode update` is an explicit signed-artifact workflow. It does not infer a release
channel, artifact, download origin, public key, current development version, or target
other than the running executable selected by `--apply`.

```sh
dacode update stable dacode-darwin-arm64 \
  --manifest-base https://releases.example/dago/ \
  --public-key /absolute/path/release-ed25519.pub

dacode update stable dacode-darwin-arm64 \
  --manifest-base https://releases.example/dago/ \
  --public-key /absolute/path/release-ed25519.pub \
  --dry-run
```

The default mode is check-only. `--dry-run` downloads and verifies the selected
artifact but performs no write. `--apply` grants executable-replacement authority and
uses the current executable unless `--target` is explicit. `--json` emits a versioned
result envelope. Development builds may use `--current vX.Y.Z` for check or dry-run,
but activation still rejects their non-release Go build identity.

The base directory is resolved to `CHANNEL.json`. It and every artifact URL must use
HTTPS. Artifact URLs must have the exact same scheme and authority as the manifest
base, and redirects are refused.

## Manifest format

The channel file is a strict JSON envelope:

```json
{
  "schema_version": 1,
  "payload": "BASE64_OF_EXACT_PAYLOAD_BYTES",
  "signature": "BASE64_ED25519_SIGNATURE_OF_PAYLOAD_BYTES"
}
```

The decoded signed payload is strict JSON:

```json
{
  "schema_version": 1,
  "channel": "stable",
  "version": "v1.2.3",
  "published_at": "2026-08-17T12:00:00Z",
  "artifacts": [
    {
      "name": "dacode-darwin-arm64",
      "url": "https://releases.example/dago/dacode-darwin-arm64",
      "sha256": "LOWERCASE_64_HEX_DIGEST",
      "size": 12345678,
      "go_package": "github.com/semistrict/dago/cmd/dacode",
      "go_module": "github.com/semistrict/dago"
    }
  ]
}
```

Sign the exact payload bytes before base64 encoding them. Unknown fields, duplicate
artifact names, invalid SemVer, mismatched channels, missing selected artifacts,
oversized content, malformed URLs, invalid signatures, checksum or length mismatches,
and Go build-provenance mismatches all fail closed. The public key file contains one
Ed25519 public key as lowercase hex or standard base64 and must be an absolute regular,
non-symbolic-link path. On Unix it must be owned by the current user and not writable
by the group or other users; Windows relies on its file ACL.

Unix can atomically rename a verified same-directory stage over the running binary;
the new release takes effect after restart. Windows cannot replace the executable that
is running the command. Run the command from a separate trusted copy and set
`--target` to the inactive installed executable, or use check/dry-run only.

## Interactive update profile

The terminal UI can use the same verifier through bare `/update`, but only when all
four trust inputs are supplied at launch:

```sh
dacode \
  --update-channel stable \
  --update-artifact dacode-darwin-arm64 \
  --update-manifest-base https://releases.example/dago/ \
  --update-public-key /absolute/path/release-ed25519.pub
```

Partial profiles and `/update` arguments are rejected without contacting a network.
The modal checks first, requires two explicit confirmations before replacement, can
cancel checking or application, maps failures to non-secret diagnostics, and retains a
completed late activation so the restart instruction is not lost. `--update-target`
may select a separate absolute executable; Windows requires that form for activation.

`/auto-update` controls an owner-private preference and defaults on when no preference
exists. `DEEPAGENTS_CODE_AUTO_UPDATE` has launch-only precedence; even an explicitly
empty value disables automatic activation for that launch. A first implicit-default
release is announced but not installed. Durable state then records consent, exact
version skips, one-launch reminders, failure cooldowns, and restart attempts so a
release is applied at most once and cannot form a restart loop. Missing update profiles,
malformed or link-backed preferences/state, and failed writes all fail closed.

Available releases are also actionable notifications. Ctrl+N opens their detail view;
Install enters the signed-update modal, Remind and Skip are persisted transactionally,
and Open changelog only opens the fixed HTTPS releases page. Generic toasts remain
visible when actionable update toasts are moved into the notification center.
