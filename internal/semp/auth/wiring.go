package auth

// WiringError is the panic value authenticator constructors use when a
// required dependency is nil. It carries the broker alias so the
// tool-handler recovery layer (internal/tools.withRecovery) can log it
// as a pre-vetted structured field.
//
// Raw panic strings cannot be logged under the secure-logging rules
// (docs/internal/secure-logging-rules.md): panic values are unaudited
// and may carry arbitrary data. A typed panic value with explicitly
// declared fields is the safe way to route a diagnostic string through
// recovery — each field's contract makes it safe to log.
//
// Kept local to internal/semp/auth: the only callers are the
// authenticator constructors in this package. If a third package needs
// the same shape, lift this to a shared location — the compiler will
// drive the rename.
type WiringError struct {
	// BrokerAlias identifies which broker's wiring tripped the panic.
	// Sourced from the broker config alias, safe to log.
	BrokerAlias string

	// Reason is the constructor-authored message describing what
	// invariant was violated (e.g. "NewOAuthAuthenticator: exchanger
	// must be non-nil"). Authored by trusted code in this package;
	// safe to log.
	Reason string
}
