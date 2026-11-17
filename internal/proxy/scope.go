package proxy

import "context"

// A listener can be confined to the sessions it serves.
//
// The proxy listens on each session network's gateway. A session's guest can
// only reach its own gateway, but every gateway is an address on this host, so
// confining a listener costs nothing and closes the case where a token that
// leaked from one session is presented on another session's network. The
// caller marks each connection's context with WithSessionScope (through
// http.Server.ConnContext), and an authenticated session outside the scope is
// refused exactly as an unknown token is.

type sessionScopeKey struct{}

// WithSessionScope returns a context under which only sessions for which
// allowed reports true authenticate. A context with no scope admits every
// session, which is what a proxy with a single shared listener does.
func WithSessionScope(ctx context.Context, allowed func(sessionID string) bool) context.Context {
	return context.WithValue(ctx, sessionScopeKey{}, allowed)
}

// inScope reports whether a session may use the listener a request arrived on.
func inScope(ctx context.Context, sessionID string) bool {
	allowed, ok := ctx.Value(sessionScopeKey{}).(func(string) bool)
	if !ok || allowed == nil {
		return true
	}
	return allowed(sessionID)
}
