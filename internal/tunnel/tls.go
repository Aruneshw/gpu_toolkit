package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"
)

// RegisterTLS performs a bounded TLS 1.3 handshake on a server-side TLS
// connection, then verifies its client certificate with authorize before
// registering it. The server TLS configuration must request and verify client
// certificates against the group's CA. authorize must independently verify
// current group membership and revocation; it must return the certificate's
// authenticated worker identity, never an identity supplied by the peer.
//
// Ownership of conn is transferred on every call: failures close it, and the
// broker owns it on success. Revocation of an existing connection still requires
// the caller to invoke Disconnect for immediate effect. While connected, the
// authorization callback is also rechecked approximately once per second;
// a rejection or changed identity disconnects that session. The callback must
// be safe for concurrent calls and must return promptly. This polling is not an
// atomic revocation guarantee. Unused connections expire after 30 seconds;
// workers renew them. Active requests have a deadline set by RoundTrip.
func (b *Broker) RegisterTLS(ctx context.Context, conn *tls.Conn, authorize func(*x509.Certificate) (string, error)) (string, error) {
	if conn == nil {
		return "", errors.New("TLS connection required")
	}
	registered := false
	defer func() {
		if !registered {
			_ = conn.Close()
		}
	}()
	if authorize == nil {
		return "", errors.New("worker authorization required")
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		return "", errors.New("worker TLS handshake failed")
	}
	state := conn.ConnectionState()
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
		return "", errors.New("verified TLS 1.3 worker certificate required")
	}
	id, err := authorize(state.PeerCertificates[0])
	if err != nil || id == "" {
		return "", errors.New("worker authorization failed")
	}
	if err := handshakeCtx.Err(); err != nil {
		return "", err
	}
	s, err := b.register(id, conn)
	if err != nil {
		return "", err
	}
	registered = true
	go func() {
		idleUntil := time.Now().Add(30 * time.Second)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				if time.Now().After(idleUntil) && b.idle(s) {
					b.disconnectSession(id, s)
					return
				}
				current, err := authorize(state.PeerCertificates[0])
				if err != nil || current != id {
					b.disconnectSession(id, s)
					return
				}
			}
		}
	}()
	return id, nil
}
