package llm

import (
	"context"

	"github.com/semistrict/dago/damodel"
)

type modelProfileCtxKeyType string

const modelProfileCtxKey modelProfileCtxKeyType = "modelProfile"

func WithModelProfile(ctx context.Context, profile damodel.Profile) context.Context {
	return context.WithValue(ctx, modelProfileCtxKey, profile)
}

func ModelProfileFromContext(ctx context.Context) (damodel.Profile, bool) {
	profile, ok := ctx.Value(modelProfileCtxKey).(damodel.Profile)
	return profile, ok
}
