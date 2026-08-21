package damodel

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

type profileTestChat struct {
	profile Profile
	bound   bool
	closed  bool
}

func (chat *profileTestChat) Invoke(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (chat *profileTestChat) Stream(context.Context, Request) (Stream, error) {
	return EmptyStream{}, nil
}
func (chat *profileTestChat) Profile() Profile { return chat.profile }
func (chat *profileTestChat) BindTools([]datool.Definition) (Chat, error) {
	copy := *chat
	copy.bound = true
	return &copy, nil
}
func (chat *profileTestChat) Close() error {
	chat.closed = true
	return nil
}

func TestEmptyStream(t *testing.T) {
	stream := EmptyStream{}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next() error = %v, want io.EOF", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestOutcomeRoundTrip(t *testing.T) {
	item := damessage.Assistant("")
	SetOutcome(&item, FinishReasonRefusal, &Refusal{Category: "policy", Explanation: "not allowed"})
	reason, refusal := Outcome(item)
	if reason != FinishReasonRefusal || refusal == nil || refusal.Category != "policy" || refusal.Explanation != "not allowed" {
		t.Fatalf("outcome = %q, %#v", reason, refusal)
	}
}

func TestWithProfilePreservesOverridesAcrossBinding(t *testing.T) {
	base := &profileTestChat{profile: Profile{Model: "base", SupportsImages: true, ReasoningLevels: []string{"low"}}}
	configured := WithProfile(base, func(profile *Profile) {
		profile.Model = "configured"
		profile.SupportsImages = false
		profile.ReasoningLevels = append(profile.ReasoningLevels, "high")
	})
	bound, err := configured.(Binder).BindTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	profile := bound.Profile()
	if profile.Model != "configured" || profile.SupportsImages || len(profile.ReasoningLevels) != 2 {
		t.Fatalf("profile = %#v", profile)
	}
	profile.ReasoningLevels[0] = "changed"
	if bound.Profile().ReasoningLevels[0] != "low" {
		t.Fatal("Profile returned mutable slice")
	}
	if !bound.(*profiledChat).Chat.(*profileTestChat).bound {
		t.Fatal("underlying model was not bound")
	}
}

func TestWithProfilePreservesModelClose(t *testing.T) {
	base := &profileTestChat{}
	configured := WithProfile(base, nil)
	closer, ok := configured.(io.Closer)
	if !ok {
		t.Fatal("profiled model lost Close")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if !base.closed {
		t.Fatal("profiled model did not close its wrapped model")
	}
}

func TestWithProfilePanicsForNilChats(t *testing.T) {
	for name, chat := range map[string]Chat{
		"nil":       nil,
		"typed nil": (*profileTestChat)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("WithProfile did not panic")
				}
			}()
			WithProfile(chat, nil)
		})
	}
}
