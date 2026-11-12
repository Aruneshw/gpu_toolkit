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
	"meshcompute/internal/config"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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
	Public               bool
	Reverse              bool
	HostEndpoint         string
}
type invitation struct {
	Endpoint        string
	CA              []byte
	Pin, Token      string
	Public, Reverse bool
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

// replaceRetry is how long Windows file operations on identity.json are
// retried. Every request reads the file and every change replaces it; on
// Windows a replace fails while the file is open elsewhere, and an open can
// fail for a moment while it is being replaced. Elsewhere both always work.
var replaceRetry = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 2 * time.Second
	}
	return 0
}()

// retry runs op until it succeeds or replaceRetry has passed.
func retry(op func() error) error {
	deadline := time.Now().Add(replaceRetry)
	for {
		err := op()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func load(dir string) (state, error) {
	var s state
	if err := secureDir(dir); err != nil {
		return s, err
	}
	p := filepath.Join(dir, "identity.json")
	var b []byte
	err := retry(func() error {
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // reported below, not retried
			}
			return err
		}
		if !fi.Mode().IsRegular() || !privatePerm(fi.Mode()) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		b, err = io.ReadAll(io.LimitReader(f, 8<<20))
		return err
	})
	if err != nil || b == nil || json.Unmarshal(b, &s) != nil {
		return state{}, fail()
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
	if retry(func() error { return os.Rename(f.Name(), filepath.Join(dir, "identity.json")) }) != nil {
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
	if ip == nil {
		return nil
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		return ip
	}
	// Tailscale and some VPNs use the RFC 6598 100.64.0.0/10 range, which
	// net.IP.IsPrivate does not include. config already admits it, so the
	// security identity layer must agree.
	if _, cgnat, _ := net.ParseCIDR("100.64.0.0/10"); cgnat.Contains(ip) {
		return ip
	}
	return nil
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
	return initIdentity(dir, host, "", false, false)
}

// InitGroup creates a creator-hosted relay identity. Public IPs are opt-in;
// the loopback-only model backend and dashboard policies are unaffected.
func InitGroup(dir, listen, advertise string, public bool) error {
	if err := config.RemoteEndpoint("https://"+listen, public); err != nil {
		return err
	}
	if err := config.RemoteEndpoint(advertise, public); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(listen)
	u, _ := url.Parse(advertise)
	return initIdentity(dir, host, u.Hostname(), public, true)
}

func initIdentity(dir, host, advertised string, public, reverse bool) error {
	ip := privateIP(host)
	if public {
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return errors.New("host must be a permitted literal IP")
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
	id := "coordinator-" + opaque()
	der, nodeKey, err := coordinatorLeaf(ca, key, id, ip, advertised, host)
	if err != nil {
		return err
	}
	return save(dir, state{ID: id, Coordinator: true, CA: certPEM(caDER), CAKey: keyPEM(key), Cert: certPEM(der), Key: keyPEM(nodeKey), Pin: digest(der), Tokens: map[string]time.Time{}, Revoked: map[string]bool{}, Public: public, Reverse: reverse})
}

// coordinatorLeaf issues the coordinator's TLS certificate for its literal
// listen IP and, when different, its advertised IP.
func coordinatorLeaf(ca *x509.Certificate, caKey any, id string, ip net.IP, advertised, host string) ([]byte, ed25519.PrivateKey, error) {
	nodePub, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fail()
	}
	leaf := template(id)
	leaf.IPAddresses = []net.IP{ip}
	if advertised != "" && advertised != host {
		adv := net.ParseIP(advertised)
		if adv == nil {
			return nil, nil, errors.New("advertised host must be a literal IP")
		}
		leaf.IPAddresses = append(leaf.IPAddresses, adv)
	}
	leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, nodePub, caKey)
	if err != nil {
		return nil, nil, fail()
	}
	return der, nodeKey, nil
}

// RetargetGroup moves an existing creator-hosted group to a new address. Home
// and mobile IPv6 prefixes change often; refusing to start would strand the
// group. The CA, coordinator ID and revocations are kept, a new coordinator
// certificate is issued for the new address, and outstanding invitations are
// discarded because they pin the old certificate. Previously enrolled workers
// must join again with a fresh invitation.
func RetargetGroup(dir, listen, advertise string, public bool) error {
	if err := config.RemoteEndpoint("https://"+listen, public); err != nil {
		return err
	}
	if err := config.RemoteEndpoint(advertise, public); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(listen)
	u, _ := url.Parse(advertise)
	ip := privateIP(host)
	if public {
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return errors.New("host must be a permitted literal IP")
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := load(dir)
	if err != nil {
		return err
	}
	if !s.Coordinator || !s.Reverse {
		return errors.New("only a group host identity can change address")
	}
	ca, err := parseCert(s.CA)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(s.CAKey)
	if block == nil {
		return fail()
	}
	caKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fail()
	}
	der, nodeKey, err := coordinatorLeaf(ca, caKey, s.ID, ip, u.Hostname(), host)
	if err != nil {
		return err
	}
	s.Cert, s.Key, s.Pin = certPEM(der), keyPEM(nodeKey), digest(der)
	s.Tokens = map[string]time.Time{}
	s.Public = public
	return save(dir, s)
}

// Pair issues a single-use invitation through local coordinator state only.
func Pair(dir, raw string, ttl time.Duration) (string, error) {
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
	if err := config.RemoteEndpoint(raw, s.Public); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
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
	b, _ := json.Marshal(invitation{Endpoint: raw, CA: s.CA, Pin: s.Pin, Token: token, Public: s.Public, Reverse: s.Reverse})
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type restrictedTransport struct {
	*http.Transport
	public bool
}

func (t restrictedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ip := net.ParseIP(r.URL.Hostname())
	if r.URL.Scheme != "https" || ip == nil || (privateIP(r.URL.Hostname()) == nil && !(t.public && ip.IsGlobalUnicast())) || r.URL.User != nil {
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
	return &http.Client{Timeout: 30 * time.Second, Transport: restrictedTransport{Transport: &http.Transport{TLSClientConfig: cfg, Proxy: nil}}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects forbidden") }}, nil
}

// Join enrolls with the exact coordinator pinned in an out-of-band invitation.
func Join(ctx context.Context, dir, code string) error {
	_, err := join(ctx, dir, code, "", false)
	return err
}

// InvitationEndpoint returns the host endpoint encoded in an invitation so
// callers can report reachability problems without confusing them with a
// rejected or expired invitation.
func InvitationEndpoint(code string) (string, bool, error) {
	if len(code) > maxInput {
		return "", false, fail()
	}
	b, err := base64.RawURLEncoding.DecodeString(code)
	var inv invitation
	if err != nil || json.Unmarshal(b, &inv) != nil || len(inv.Token) != 64 {
		return "", false, fail()
	}
	return inv.Endpoint, inv.Public && inv.Reverse, nil
}

// JoinHost additionally enables a worker listener at an explicit private IP.
func JoinHost(ctx context.Context, dir, code, host string) error {
	if privateIP(host) == nil {
		return errors.New("host must be a private or loopback IP")
	}
	_, err := join(ctx, dir, code, host, false)
	return err
}

// RejoinHost enrolls with a new invitation even when this directory already
// holds a worker identity, for example after the group host's address changed.
// The previous identity is kept as identity.json.bak-TIMESTAMP and is only
// moved after the new enrollment succeeds. A coordinator identity is never
// replaced. It returns the backup path, or "" when nothing was replaced.
func RejoinHost(ctx context.Context, dir, code, host string) (string, error) {
	if privateIP(host) == nil {
		return "", errors.New("host must be a private or loopback IP")
	}
	return join(ctx, dir, code, host, true)
}

// HostEndpoint returns the group host a worker identity connects to.
func HostEndpoint(dir string) (string, error) {
	s, err := load(dir)
	if err != nil {
		return "", err
	}
	return s.HostEndpoint, nil
}

func join(ctx context.Context, dir, code, host string, replace bool) (backup string, err error) {
	err = joinLocked(ctx, dir, code, host, replace, &backup)
	return backup, err
}

func joinLocked(ctx context.Context, dir, code, host string, replace bool, backup *string) error {
	if len(code) > maxInput {
		return fail()
	}
	b, err := base64.RawURLEncoding.DecodeString(code)
	var inv invitation
	if err != nil || json.Unmarshal(b, &inv) != nil || len(inv.Token) != 64 {
		return errors.New("the invitation is malformed or truncated; paste the whole code")
	}
	err = config.RemoteEndpoint(inv.Endpoint, inv.Public && inv.Reverse)
	if err != nil {
		return err
	}
	u, _ := url.Parse(inv.Endpoint)
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	existing := !absent(dir)
	if existing {
		if !replace {
			return errors.New("identity already exists")
		}
		old, err := load(dir)
		if err != nil {
			return err
		}
		if old.Coordinator {
			return errors.New("this directory holds a group host or coordinator identity; join from a separate --home")
		}
	}
	client, err := transport(inv.CA, inv.Pin, nil)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	tr := client.Transport.(restrictedTransport)
	tr.public = inv.Public && inv.Reverse
	client.Transport = tr
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
		return errors.New("could not complete a pinned TLS connection to the group host")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("the group host refused this invitation")
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
	if existing {
		*backup = filepath.Join(dir, "identity.json.bak-"+time.Now().UTC().Format("20060102-150405"))
		if err := os.Rename(filepath.Join(dir, "identity.json"), *backup); err != nil {
			*backup = ""
			return fail()
		}
	}
	return save(dir, state{ID: cred.ID, CA: inv.CA, Cert: cred.Cert, Key: keyPEM(key), Pin: inv.Pin, Public: inv.Public && inv.Reverse, Reverse: inv.Reverse, HostEndpoint: inv.Endpoint})
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
	rt := c.Transport.(restrictedTransport)
	rt.public = s.Public
	c.Transport = rt
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

// VerifyWorkerCertificate verifies an enrolled worker against the current
// coordinator CA and revocation state. It is intended for an authenticated
// tunnel handshake; callers must repeat authorization when appropriate and
// disconnect existing tunnels after revocation. It does not perform a TLS
// handshake or establish proof of possession by itself.
func VerifyWorkerCertificate(dir string, cert *x509.Certificate) (string, error) {
	if cert == nil || cert.IsCA {
		return "", fail()
	}
	id := cert.Subject.CommonName
	if !strings.HasPrefix(id, "worker-") || len(id) != 71 {
		return "", fail()
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(id, "worker-")); err != nil {
		return "", fail()
	}
	s, err := load(dir)
	if err != nil || !s.Coordinator || s.Revoked[id] {
		return "", fail()
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(s.CA) {
		return "", fail()
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", fail()
	}
	return id, nil
}

// NodeID reads the local stable identity.
func NodeID(dir string) (string, error) {
	s, err := load(dir)
	if err != nil {
		return "", err
	}
	return s.ID, nil
}
