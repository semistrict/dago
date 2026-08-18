# WhatsApp channel

`datalon/whatsapp` attaches a `datalon.Channel` to the packaged Node bridge for
the pinned Talon WhatsApp behavior. The Go side requires a transport and a
stable session directory positionally; construction performs no I/O. The
built-in transport accepts only authenticated loopback HTTP origins and pins
`localhost` to a numeric loopback address without proxy or redirect use.

```go
transport := whatsapp.NewHTTPTransport(
	"http://127.0.0.1:3000",
	os.Getenv("WHATSAPP_BRIDGE_TOKEN"),
	whatsapp.HTTPOptions{},
)
channel := whatsapp.New(transport, "/private/operator/whatsapp-session", whatsapp.Options{
	Exposure: whatsapp.Exposure{
		Mode:        whatsapp.ExposureSelf,
		OperatorIDs: []string{"15551234567@c.us"},
	},
})
host := datalon.NewHost(runtime, config, channel)
```

Self exposure is the default and accepts only provider-authenticated self
messages or configured operator IDs. Allowlist exposure accepts exact
conversation IDs or case-sensitive `*`/`?` mention patterns. Open exposure
requires the exact `whatsapp.OpenAcknowledgement` value. Every outbound text
chunk carries the `deepagents bot` header by default.

## Bridge operation

Install and verify the packaged bridge with pnpm:

```sh
cd datalon/whatsapp/bridge
pnpm install --frozen-lockfile
pnpm test
```

Start `bridge.js` with `WHATSAPP_BRIDGE_TOKEN` and, for production, explicit
`WHATSAPP_SESSION_DIR` and `WHATSAPP_MEDIA_DIR` values. The listener defaults to
`127.0.0.1:3000`; non-loopback host values are rejected. During first pairing,
the bridge reports `qr_pending` and prints the QR code to its operator-owned
standard output. Session credentials persist in the private session directory.

The application owns the Node process lifecycle. This differs from the pinned
Python adapter's optional subprocess command, and keeps process supervision,
logging, Chrome installation, restart policy, and secrets outside the Go
channel. The bridge uses `whatsapp-web.js`; it does not provide an official
WhatsApp Business API implementation.

## Security and bounds

The bearer token authenticates the Go client to the local bridge; the caller
must supply it through protected process configuration and must not log it.
Inbound JSON, queue count and bytes, message fields, poll batches, errors, and
outbound chunks have finite defaults. Media is capped at 64 MiB even when a
larger value is requested. Downloaded and staged media stays under the private
bridge media directory; outbound local files must resolve beneath the
caller-selected outbound root, and symlink escapes and non-regular files are
rejected.

`Start` authenticates with `/health` before background polling begins. A custom
`Transport` must be caller-authenticated, honor cancellation, and apply limits
before allocating response data; the channel defensively rejects oversized
returned payloads but cannot reclaim an allocation already made by a broken
transport. Neither the bridge token nor WhatsApp session data belongs in
messages or model-visible metadata.
