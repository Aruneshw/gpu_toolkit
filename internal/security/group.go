package security

import (
	"crypto/tls"
	"net/http"
)

// Group returns nonsecret connection settings stored with the local identity.
func Group(dir string) (host bool, reverse bool, endpoint string, err error) {
	s, err := load(dir)
	if err != nil {
		return false, false, "", err
	}
	return s.Coordinator, s.Reverse, s.HostEndpoint, nil
}

// WorkerTLS returns the worker's mutual TLS credentials with exact host pinning.
func WorkerTLS(dir string) (*tls.Config, error) {
	s, err := load(dir)
	if err != nil || s.Coordinator || !s.Reverse {
		return nil, fail()
	}
	c, err := Client(dir)
	if err != nil {
		return nil, err
	}
	cfg := c.Transport.(restrictedTransport).TLSClientConfig.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || digest(cs.PeerCertificates[0].Raw) != s.Pin {
			return fail()
		}
		return nil
	}
	return cfg, nil
}

// SelfOnly protects local host proxy operations from every enrolled worker.
// The dashboard/CLI use the local certificate; no remote administration exists.
func SelfOnly(dir string, next http.Handler) http.Handler {
	return Authorize(dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := load(dir)
		if err != nil || !s.Coordinator {
			http.Error(w, "access denied", 403)
			return
		}
		cert, err := parseCert(s.Cert)
		if err != nil || digest(r.TLS.PeerCertificates[0].Raw) != digest(cert.Raw) {
			http.Error(w, "access denied", 403)
			return
		}
		next.ServeHTTP(w, r)
	}))
}
