package handlers

import "context"

// authenticatedUserContextKey is intentionally private so callers cannot
// accidentally collide with the public API identity value.
type authenticatedUserContextKey struct{}

// WithAuthenticatedUser stores the already validated public-API principal in
// the request context. The authn middleware owns validation; handlers only use
// this value for immutable metadata such as created-by.
func WithAuthenticatedUser(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, authenticatedUserContextKey{}, email)
}

// AuthenticatedUserFromContext returns the validated public-API principal.
func AuthenticatedUserFromContext(ctx context.Context) (string, bool) {
	email, ok := ctx.Value(authenticatedUserContextKey{}).(string)
	return email, ok && email != ""
}
