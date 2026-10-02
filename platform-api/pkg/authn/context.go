package authn

import "context"

type userContextKey struct{}

// WithUser stores the normalized authenticated principal in a request
// context.
func WithUser(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, userContextKey{}, email)
}

// UserFromContext returns the normalized authenticated principal.
func UserFromContext(ctx context.Context) (string, bool) {
	email, ok := ctx.Value(userContextKey{}).(string)
	return email, ok && email != ""
}
