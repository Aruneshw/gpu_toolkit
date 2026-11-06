// Package security manages local cluster identities and pinned HTTPS enrollment.
package security

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxInput = 64 << 10
const enrollPath = "/enroll"

type state struct {
	ID                   string
	Coordinator          bool
	CA, CAKey, Cert, Key []byte
	Pin                  string
	Tokens               map[string]time.Time
	Revoked              map[string]bool
}
type invitation struct {
	Endpoint   string
	CA         []byte
	Pin, Token string
}
type enrollment struct {
	Token string
	CSR   []byte
	Host  string
}
type credential struct {
	ID   string
	Cert []byte
}

func opaque() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func fail() error            { return errors.New("security operation failed") }

func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fail()
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || !privatePerm(fi.Mode()) {
		return errors.New("identity directory must be private (0700)")
	}
	return nil
}
func lock(dir string) (func(), error) {
	if err := secureDir(dir); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, ".lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(p, 0700)
		if err == nil {
			return func() { _ = os.Remove(p) }, nil
		}
		if !os.IsExist(err) {
			return nil, fail()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("identity directory busy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func load(dir string) (state, error) {
	var s state
	if err := secureDir(dir); err != nil {
		return s, err
	}
	p := filepath.Join(dir, "identity.json")
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || !privatePerm(fi.Mode()) {
		return s, fail()
	}
	f, err := os.Open(p)
	if err != nil {
		return s, fail()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, fail()
	}
	return s, nil
}
func save(dir string, s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fail()
	}
	if len(b) >= 8<<20 {
		return fail()
	}
	f, err := os.CreateTemp(dir, ".identity-")
	if err != nil {
		return fail()
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fail()
	}
	if os.Rename(f.Name(), filepath.Join(dir, "identity.json")) != nil {
		return fail()
	}
	return syncDir(dir)
}
func absent(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, "identity.json"))
	return os.IsNotExist(err)
}
func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func parseCert(b []byte) (*x509.Certificate, error) {
	p, _ := pem.Decode(b)
	if p == nil {
		return nil, fail()
	}
	c, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return nil, fail()
	}
	return c, nil
}
func keyPEM(k ed25519.PrivateKey) []byte {
	b, _ := x509.MarshalPKCS8PrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
}
func template(id string) *x509.Certificate {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: id}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
}
func privateIP(host string) net.IP {
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) {
		return nil
	}
	return ip
}
func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || privateIP(u.Hostname()) == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be an HTTPS private or loopback IP origin")
	}
	if p := u.Port(); p != "" {
		if _, err := net.LookupPort("tcp", p); err != nil {
			return nil, fail()
		}
	}
	return u, nil
}

// Init creates a coordinator and its retained CA; host is a private or loopback IP.
func Init(dir, host string) error {
	ip := privateIP(host)
	if ip == nil {
		return errors.New("host must be a private or loopback IP")
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if !absent(dir) {
		return errors.New("identity already exists")
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fail()
	}
	ca := template("cluster-" + opaque())
	ca.IsCA = true
	ca.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	ca.NotAfter = time.Now().Add(5 * 365 * 24 * time.Hour)
	ca.MaxPathLenZero = true
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		return fail()
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		return fail()
	}
	nodePub, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fail()
	}
	id := "coordinator-" + opaque()
	leaf := template(id)
	leaf.IPAddresses = []net.IP{ip}
	leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, nodePub, key)
	if err != nil {
		return fail()
	}
	return save(dir, state{ID: id, Coordinator: true, CA: certPEM(caDER), CAKey: keyPEM(key), Cert: certPEM(der), Key: keyPEM(nodeKey), Pin: digest(der), Tokens: map[string]time.Time{}, Revoked: map[string]bool{}})
}

// Pair issues a single-use invitation through local coordinator state only.
func Pair(dir, raw string, ttl time.Duration) (string, error) {
	u, err := endpoint(raw)
	if err != nil {
		return "", err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return "", errors.New("token lifetime must be positive and at most 24 hours")
	}
	unlock, err := lock(dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := load(dir)
	if err != nil || !s.Coordinator {
		return "", fail()
	}
	c, err := parseCert(s.Cert)
	if err != nil || c.VerifyHostname(u.Hostname()) != nil {
		return "", fail()
	}
	for k, t := range s.Tokens {
		if !t.After(time.Now()) {
			delete(s.Tokens, k)
		}
	}
	if s.Tokens == nil {
		s.Tokens = map[string]time.Time{}
	}
	token := opaque()
	s.Tokens[digest([]byte(token))] = time.Now().Add(ttl)
	if err = save(dir, s); err != nil {
		return "", err
	}
	b, _ := json.Marshal(invitation{Endpoint: raw, CA: s.CA, Pin: s.Pin, Token: token})
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type restrictedTransport struct{ *http.Transport }

func (t restrictedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || privateIP(r.URL.Hostname()) == nil || r.URL.User != nil {
		return nil, errors.New("only HTTPS private or loopback IP requests are permitted")
	}
	return t.Transport.RoundTrip(r)
}
func transport(ca []byte, pin string, cert *tls.Certificate) (*http.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) || len(pin) != 64 {
		return nil, fail()
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || digest(cs.PeerCertificates[0].Raw) != pin {
			return fail()
		}
		return nil
	}}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: restrictedTransport{&http.Transport{TLSClientConfig: cfg, Proxy: nil}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects forbidden") }}, nil
}

// Join enrolls with the exact coordinator pinned in an out-of-band invitation.
func Join(ctx context.Context, dir, code string) error {
	return join(ctx, dir, code, "")
}

// JoinHost additionally enables a worker listener at an explicit private IP.
func JoinHost(ctx context.Context, dir, code, host string) error {
	if privateIP(host) == nil {
		return errors.New("host must be a private or loopback IP")
	}
	return join(ctx, dir, code, host)
}

func join(ctx context.Context, dir, code, host string) error {
	if len(code) > maxInput {
		return fail()
	}
	b, err := base64.RawURLEncoding.DecodeString(code)
	var inv invitation
	if err != nil || json.Unmarshal(b, &inv) != nil || len(inv.Token) != 64 {
		return fail()
	}
	u, err := endpoint(inv.Endpoint)
	if err != nil {
		return err
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if !absent(dir) {
		return errors.New("identity already exists")
	}
	client, err := transport(inv.CA, inv.Pin, nil)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fail()
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return fail()
	}
	body, _ := json.Marshal(enrollment{Token: inv.Token, CSR: csr, Host: host})
	u.Path = enrollPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fail()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fail()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail()
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInput+1))
	var cred credential
	if err != nil || len(data) > maxInput || json.Unmarshal(data, &cred) != nil {
		return fail()
	}
	pair, err := tls.X509KeyPair(cred.Cert, keyPEM(key))
	if err != nil {
		return fail()
	}
	c, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || c.Subject.CommonName != cred.ID || !strings.HasPrefix(cred.ID, "worker-") {
		return fail()
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(inv.CA)
	if _, err = c.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fail()
	}
	if host != "" && c.VerifyHostname(host) != nil {
		return fail()
	}
	return save(dir, state{ID: cred.ID, CA: inv.CA, Cert: cred.Cert, Key: keyPEM(key), Pin: inv.Pin})
}

// ServerTLS permits unauthenticated enrollment; wrap every other route in Authorize.
func ServerTLS(dir string) (*tls.Config, error) {
	s, err := load(dir)
	if err != nil {
		return nil, fail()
	}
	cert, err := tls.X509KeyPair(s.Cert, s.Key)
	if err != nil {
		return nil, fail()
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(s.CA) {
		return nil, fail()
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.VerifyClientCertIfGiven}
	if !s.Coordinator {
		leaf, e := parseCert(s.Cert)
		if e != nil || len(leaf.IPAddresses) == 0 {
			return nil, fail()
		}
		own := digest(leaf.Raw)
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fail()
			}
			d := digest(cs.PeerCertificates[0].Raw)
			// Accept the coordinator and this node's own loopback identity.
			if d != s.Pin && d != own {
				return fail()
			}
			return nil
		}
	}
	return cfg, nil
}

// Client trusts only the coordinator on workers, and non-revoked workers on coordinators.
func Client(dir string) (*http.Client, error) {
	s, err := load(dir)
	if err != nil {
		return nil, fail()
	}
	cert, err := tls.X509KeyPair(s.Cert, s.Key)
	if err != nil {
		return nil, fail()
	}
	c, err := transport(s.CA, s.Pin, &cert)
	if err != nil {
		return nil, err
	}
	t := c.Transport.(restrictedTransport).Transport
	// A fresh handshake prevents pooled connections from bypassing revocation.
	t.DisableKeepAlives = true
	if s.Coordinator {
		t.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			current, e := load(dir)
			if e != nil || !current.Coordinator || len(cs.PeerCertificates) == 0 {
				return fail()
			}
			leaf := cs.PeerCertificates[0]
			own, e := parseCert(current.Cert)
			if e == nil && digest(leaf.Raw) == digest(own.Raw) {
				return nil
			}
			if !strings.HasPrefix(leaf.Subject.CommonName, "worker-") || leaf.IsCA || current.Revoked[leaf.Subject.CommonName] {
				return fail()
			}
			return nil
		}
	} else {
		own, e := parseCert(s.Cert)
		if e != nil {
			return nil, fail()
		}
		t.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fail()
			}
			d := digest(cs.PeerCertificates[0].Raw)
			// Accept the coordinator and this node's own loopback identity.
			if d != s.Pin && d != digest(own.Raw) {
				return fail()
			}
			return nil
		}
	}
	return c, nil
}

// EnrollHandler serves POST /enroll. No administrative operations are exposed.
func EnrollHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reject := func() { http.Error(w, "enrollment rejected", http.StatusForbidden) }
		if r.TLS == nil || r.Method != http.MethodPost || r.URL.Path != enrollPath {
			reject()
			return
		}
		var e enrollment
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInput))
		if err != nil || json.Unmarshal(b, &e) != nil || len(e.Token) != 64 || (e.Host != "" && privateIP(e.Host) == nil) {
			reject()
			return
		}
		csr, err := x509.ParseCertificateRequest(e.CSR)
		if err != nil || csr.CheckSignature() != nil {
			reject()
			return
		}
		if _, ok := csr.PublicKey.(ed25519.PublicKey); !ok {
			reject()
			return
		}
		unlock, err := lock(dir)
		if err != nil {
			reject()
			return
		}
		defer unlock()
		s, err := load(dir)
		if err != nil || !s.Coordinator {
			reject()
			return
		}
		hash := digest([]byte(e.Token))
		if !s.Tokens[hash].After(time.Now()) {
			reject()
			return
		}
		ca, err := parseCert(s.CA)
		if err != nil {
			reject()
			return
		}
		block, _ := pem.Decode(s.CAKey)
		if block == nil {
			reject()
			return
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			reject()
			return
		}
		id := "worker-" + opaque()
		leaf := template(id)
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		if e.Host != "" {
			leaf.IPAddresses = []net.IP{privateIP(e.Host)}
			leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, key)
		if err != nil {
			reject()
			return
		}
		delete(s.Tokens, hash)
		if save(dir, s) != nil {
			reject()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(credential{id, certPEM(der)})
	})
}

// Authorize checks the opposite role and reloads local policy on every request.
func Authorize(dir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func() { http.Error(w, "access denied", http.StatusForbidden) }
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			deny()
			return
		}
		s, err := load(dir)
		if err != nil {
			deny()
			return
		}
		c := r.TLS.PeerCertificates[0]
		own, e := parseCert(s.Cert)
		if e != nil {
			deny()
			return
		}
		self := digest(c.Raw) == digest(own.Raw)
		if c.IsCA || (s.Coordinator && !self && (!strings.HasPrefix(c.Subject.CommonName, "worker-") || s.Revoked[c.Subject.CommonName])) || (!s.Coordinator && !self && digest(c.Raw) != s.Pin) {
			deny()
			return
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(s.CA)
		if _, err := c.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			deny()
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Revoke persistently denies a worker ID, using local coordinator state only.
func Revoke(dir, id string) error {
	if !strings.HasPrefix(id, "worker-") || len(id) != 71 {
		return errors.New("invalid worker ID")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(id, "worker-")); err != nil {
		return errors.New("invalid worker ID")
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := load(dir)
	if err != nil || !s.Coordinator {
		return fail()
	}
	if s.Revoked == nil {
		s.Revoked = map[string]bool{}
	}
	s.Revoked[id] = true
	return save(dir, s)
}

// NodeID reads the local stable identity.
func NodeID(dir string) (string, error) {
	s, err := load(dir)
	if err != nil {
		return "", err
	}
	return s.ID, nil
}
