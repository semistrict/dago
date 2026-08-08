---
name: request-integration
description: Use when a task needs an external service credential (API key, database, OAuth service) this exe.dev VM doesn't have attached. Emits a clickable exe.dev connect link the user clicks to grant it. NEVER ask the user to paste a secret into chat.
when: exe.dev
---

# Requesting an integration the VM doesn't have

**Core rule — the reason this skill exists:** NEVER ask the user to paste a
secret (API key, token, password, connection string) into the chat. Secrets
must not land in the conversation or on the VM. The clickable connect link
below is the ONLY handoff: exe.dev collects the credential in its own dialog
(POST body, never a URL) and injects it at the network edge.

## Steps

1. **See what's already attached.** Don't ask for something the VM already has.
   ```
   curl https://reflection.int.exe.xyz/integrations
   ```
   (The `reflection-integration` skill covers this endpoint in more detail.)

2. **Find the service handle in the catalog.** The connect link needs a
   catalog handle (e.g. `stripe`, `gmail`, `github`).
   ```
   curl https://exe.dev/docs/integrations-catalog.md
   ```
   NOTE: this generated catalog page is a planned discovery surface and may not
   exist yet (it 404s until the docs leg ships) and, once it exists, may lag the
   live catalog. If it's missing or the service isn't listed, emit the link
   anyway with your best-guess handle — the add page degrades gracefully: an
   unknown handle opens the catalog browse pre-filled with a search, and a true
   catalog gap lands the user on the "suggest a service" form. A wrong guess
   never breaks anything.

3. **Get this VM's name** (for the `attach=vm:` spec):
   ```
   curl https://reflection.int.exe.xyz/    # read the .name field
   ```

4. **Emit the connect link in conversation** and describe, in plain language,
   what connecting does and exactly what you'll use it for — the user should
   not be surprised after they click (the suggest-links "Notes for agents"
   convention):
   ```
   https://exe.dev/integrations/add?service=<handle>&attach=vm:<this-vm>&for=<duration>&source=shelley
   ```
   - `<handle>`: the catalog handle from step 2.
   - `<this-vm>`: the `.name` from step 3.
   - `for=<duration>`: a positive Go duration (`2h`, `45m`, `24h`). Ask for the
     SHORTEST window that covers the task — the grant auto-lapses at expiry,
     nothing to revoke. Omit `for=` only if the user explicitly wants the grant
     to be permanent.
   - `source=shelley` is fixed telemetry; leave it as-is.

   The user clicks the link, lands in the pre-filled add dialog, pastes the
   credential there (or clicks "Authorize" for OAuth services), verifies, and
   submits. Nothing is granted until they submit — the link is safe to share
   and safe to forge.

5. **After the user confirms they've connected, re-check** so you proceed on
   fact, not on their say-so:
   ```
   curl https://reflection.int.exe.xyz/integrations
   ```

## Notes

- Again: do NOT accept a secret in chat, and do NOT write one to the VM. If the
  user tries to paste a key, stop them and point them back at the connect link.
- For CLI-shaped adds the user runs themselves (not credential-bearing), the
  older `https://exe.dev/suggest?command=<url-encoded-command>` handoff still
  works, but it is allowlisted to a small fixed set of commands and can never
  carry a secret. Prefer the connect link above for anything credential-shaped.
