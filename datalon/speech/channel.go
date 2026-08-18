package speech

import (
	"context"
	"fmt"

	"github.com/semistrict/dago/datalon"
)

// ChannelOptions controls wrapper failure behavior and transcript size. By
// default transcription failures are reported to OnError and the original
// message continues to the host.
type ChannelOptions struct {
	Strict             bool
	MaxTranscriptBytes int
	OnError            func(error)
}

// Channel adds best-effort transcription to another channel.
type Channel struct {
	inner       datalon.Channel
	transcriber Transcriber
	options     ChannelOptions
}

// NewChannel constructs a transcription wrapper. Both managed dependencies are
// required positional values and typed nil values panic.
func NewChannel(channel datalon.Channel, transcriber Transcriber, options ChannelOptions) *Channel {
	if nilValue(channel) {
		panic("datalon/speech: nil channel")
	}
	if nilValue(transcriber) {
		panic("datalon/speech: nil transcriber")
	}
	if options.MaxTranscriptBytes < 0 {
		panic("datalon/speech: transcript limit cannot be negative")
	}
	if options.MaxTranscriptBytes == 0 {
		options.MaxTranscriptBytes = 64 << 10
	}
	if options.MaxTranscriptBytes > 1<<20 {
		panic("datalon/speech: transcript limit exceeds 1 MiB")
	}
	return &Channel{inner: channel, transcriber: transcriber, options: options}
}

func (channel *Channel) ID() string { return channel.inner.ID() }

func (channel *Channel) Start(ctx context.Context, handler datalon.Handler) error {
	if handler == nil {
		return fmt.Errorf("start speech channel: handler is required")
	}
	return channel.inner.Start(ctx, func(callCtx context.Context, message datalon.Message) error {
		updated, err := TranscribeMessage(callCtx, channel.transcriber, message, channel.options.MaxTranscriptBytes)
		if err != nil {
			if channel.options.OnError != nil {
				channel.options.OnError(err)
			}
			if channel.options.Strict {
				return err
			}
			updated = message
		}
		return handler(callCtx, updated)
	})
}

func (channel *Channel) Stop(ctx context.Context) error {
	return channel.inner.Stop(ctx)
}

func (channel *Channel) Send(ctx context.Context, conversationID, text string) datalon.SendResult {
	return channel.inner.Send(ctx, conversationID, text)
}
