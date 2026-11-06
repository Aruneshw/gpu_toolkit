package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"meshcompute/internal/tunnel"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivateIP(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.1.2", "fd00::1", "100.64.0.1", "100.127.255.254"} {
		if privateIP(host) == nil {
			t.Errorf("rejected private address %s", host)
		}
	}
	for _, host := range []string{"8.8.8.8", "100.63.255.255", "100.128.0.1", "2001:4860:4860::8888", "example.com", ""} {
		if privateIP(host) != nil {
			t.Errorf("accepted non-private address %s", host)
		}
	}
}

func TestAuthenticatedTunnelRegistration(t *testing.T) {
	dir, srv := coordinator(t)
	workerDir := worker(t, dir, srv.URL)
	otherDir, otherSrv := coordinator(t)
	foreignDir := worker(t, otherDir, otherSrv.URL)
	hostState, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(hostState.CA) {
		t.Fatal("host CA unavailable")
	}
	workerState, err := load(workerDir)
	if err != nil {
		t.Fatal(err)
	}
	foreignState, err := load(foreignDir)
	if err != nil {
		t.Fatal(err)
	}
	pair := func(s state) tls.Certificate {
		p, err := tls.X509KeyPair(s.Cert, s.Key)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	workerCert, foreignCert, hostCert := pair(workerState), pair(foreignState), pair(hostState)
	broker, err := tunnel.NewBroker(4)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()

	type result struct {
		id  string
		err error
	}
	attempt := func(cert *tls.Certificate, verifyClients bool) (*tls.Conn, result) {
		t.Helper()
		serverConfig, err := ServerTLS(dir)
		if err != nil {
			t.Fatal(err)
		}
		if verifyClients {
			serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			serverConfig.ClientAuth = tls.RequestClientCert
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		results := make(chan result, 1)
		go func() {
			raw, err := listener.Accept()
			if err != nil {
				results <- result{err: err}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, err := broker.RegisterTLS(ctx, tls.Server(raw, serverConfig), func(c *x509.Certificate) (string, error) {
				return VerifyWorkerCertificate(dir, c)
			})
			results <- result{id, err}
		}()
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "127.0.0.1"}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		client, clientErr := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", listener.Addr().String(), cfg)
		select {
		case r := <-results:
			if clientErr != nil && r.err == nil {
				t.Fatalf("client failed but server accepted: %v", clientErr)
			}
			return client, r
		case <-time.After(7 * time.Second):
			t.Fatal("tunnel registration timed out")
			return nil, result{}
		}
	}

	client, r := attempt(&workerCert, true)
	if r.err != nil || r.id != workerState.ID || client == nil {
		t.Fatalf("valid worker rejected: %+v", r)
	}
	conn, release, err := broker.Acquire(context.Background(), workerState.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	writeDone := make(chan error, 1)
	go func() { _, err := client.Write([]byte("ping")); writeDone <- err }()
	var buf [4]byte
	if _, err := io.ReadFull(conn, buf[:]); err != nil || string(buf[:]) != "ping" {
		t.Fatalf("tunnel transfer: %q %v", buf[:], err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	release()
	client.Close()

	for name, tc := range map[string]struct {
		cert   *tls.Certificate
		verify bool
	}{
		"foreign group":           {&foreignCert, true},
		"no certificate":          {nil, true},
		"coordinator certificate": {&hostCert, true},
		"unverified TLS":          {&workerCert, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := attempt(tc.cert, tc.verify)
			if c != nil {
				defer c.Close()
			}
			if got.err == nil {
				broker.Disconnect(got.id)
				t.Fatalf("accepted invalid worker: %s", got.id)
			}
		})
	}
	activeClient, active := attempt(&workerCert, true)
	if active.err != nil || activeClient == nil {
		t.Fatalf("second connection rejected: %v", active.err)
	}
	defer activeClient.Close()
	_, activeRelease, err := broker.Acquire(context.Background(), workerState.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer activeRelease()
	if err := Revoke(dir, workerState.ID); err != nil {
		t.Fatal(err)
	}
	if err := activeClient.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := activeClient.Read(one[:]); err == nil {
		t.Fatal("revoked active worker connection remained open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("active worker was not disconnected after revocation: %v", err)
	}
	client, r = attempt(&workerCert, true)
	if client != nil {
		defer client.Close()
	}
	if r.err == nil {
		broker.Disconnect(r.id)
		t.Fatal("accepted revoked worker")
	}
}

func coordinator(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	d := filepath.Join(t.TempDir(), "identity")
	if err := Init(d, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/enroll", EnrollHandler(d))
	mux.Handle("/", Authorize(d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })))
	cfg, err := ServerTLS(d)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return d, srv
}
func invite(t *testing.T, d, u string) string {
	t.Helper()
	c, e := Pair(d, u, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func worker(t *testing.T, d, u string) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "worker")
	if e := Join(context.Background(), w, invite(t, d, u)); e != nil {
		t.Fatal(e)
	}
	return w
}
func status(t *testing.T, c *http.Client, u string) int {
	t.Helper()
	r, e := c.Get(u)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	io.Copy(io.Discard, r.Body)
	return r.StatusCode
}
func decode(t *testing.T, code string) invitation {
	t.Helper()
	b, e := base64.RawURLEncoding.DecodeString(code)
	if e != nil {
		t.Fatal(e)
	}
	var i invitation
	if e = json.Unmarshal(b, &i); e != nil {
		t.Fatal(e)
	}
	return i
}
func encode(i invitation) string {
	b, _ := json.Marshal(i)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestEnrollmentAuthorizationRevocation(t *testing.T) {
	d, s := coordinator(t)
	code := invite(t, d, s.URL)
	inv := decode(t, code)
	raw, _ := os.ReadFile(filepath.Join(d, "identity.json"))
	if strings.Contains(string(raw), inv.Token) {
		t.Fatal("plaintext token persisted")
	}
	w := filepath.Join(t.TempDir(), "worker")
	if e := Join(context.Background(), w, code); e != nil {
		t.Fatal(e)
	}
	if e := Join(context.Background(), filepath.Join(t.TempDir(), "replay"), code); e == nil {
		t.Fatal("replay accepted")
	}
	ws, e := load(w)
	if e != nil || len(ws.CAKey) != 0 {
		t.Fatal("worker has CA key")
	}
	id, e := NodeID(w)
	if e != nil || id != ws.ID {
		t.Fatal("identity mismatch")
	}
	c, e := Client(w)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	if got := status(t, c, s.URL); got != 200 {
		t.Fatal(got)
	}
	if e := Revoke(d, id); e != nil {
		t.Fatal(e)
	}
	if got := status(t, c, s.URL); got != 403 {
		t.Fatal(got)
	}
	if _, e := Pair(w, s.URL, time.Minute); e == nil {
		t.Fatal("worker paired")
	}
	if e := Revoke(w, id); e == nil {
		t.Fatal("worker revoked")
	}
	if _, e := ServerTLS(w); e == nil {
		t.Fatal("worker server enabled")
	}
	if _, e := Client(d); e != nil {
		t.Fatal(e)
	}
	if e := Init(d, "127.0.0.1"); e == nil {
		t.Fatal("overwrote identity")
	}
	for _, p := range []string{d, filepath.Join(d, "identity.json"), w, filepath.Join(w, "identity.json")} {
		fi, e := os.Stat(p)
		if e != nil || !privatePerm(fi.Mode()) {
			t.Fatal("insecure permissions", p)
		}
	}
}
func TestAnonymousAndMalformed(t *testing.T) {
	d, s := coordinator(t)
	st, _ := load(d)
	c, _ := transport(st.CA, st.Pin, nil)
	defer c.CloseIdleConnections()
	if got := status(t, c, s.URL); got != 403 {
		t.Fatal(got)
	}
	for _, body := range []string{"{}", "not json", strings.Repeat("x", maxInput+1)} {
		r, e := c.Post(s.URL+enrollPath, "application/json", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatal(r.StatusCode)
		}
	}
	if got := status(t, c, s.URL+enrollPath); got != 403 {
		t.Fatal(got)
	}
	rr := httptest.NewRecorder()
	EnrollHandler(d).ServeHTTP(rr, httptest.NewRequest("POST", "/enroll", strings.NewReader("{}")))
	if rr.Code != 403 {
		t.Fatal(rr.Code)
	}
}
func TestExpiredAndInvalidInvitations(t *testing.T) {
	d, s := coordinator(t)
	code := invite(t, d, s.URL)
	i := decode(t, code)
	st, _ := load(d)
	st.Tokens[digest([]byte(i.Token))] = time.Now().Add(-time.Second)
	if e := save(d, st); e != nil {
		t.Fatal(e)
	}
	for _, code := range []string{code, "!", strings.Repeat("a", maxInput+1)} {
		if e := Join(context.Background(), filepath.Join(t.TempDir(), "w"), code); e == nil {
			t.Fatal("invalid invitation accepted")
		}
	}
	for _, u := range []string{"http://127.0.0.1", "https://example.com", "https://8.8.8.8", "https://127.0.0.1/path", "https://user@127.0.0.1", "https://127.0.0.1?token=x", "https://127.0.0.2"} {
		if _, e := Pair(d, u, time.Minute); e == nil {
			t.Fatal("bad endpoint", u)
		}
	}
	for _, ttl := range []time.Duration{0, -1, 25 * time.Hour} {
		if _, e := Pair(d, s.URL, ttl); e == nil {
			t.Fatal("bad TTL")
		}
	}
}
func TestPinAndForeignCluster(t *testing.T) {
	d, s := coordinator(t)
	d2, s2 := coordinator(t)
	code := invite(t, d, s.URL)
	i := decode(t, code)
	i.Endpoint = s2.URL
	if e := Join(context.Background(), filepath.Join(t.TempDir(), "w"), encode(i)); e == nil {
		t.Fatal("foreign CA accepted")
	}
	i = decode(t, code)
	i.Pin = strings.Repeat("0", 64)
	if e := Join(context.Background(), filepath.Join(t.TempDir(), "w"), encode(i)); e == nil {
		t.Fatal("wrong pin accepted")
	}
	w := worker(t, d, s.URL)
	c, _ := Client(w)
	defer c.CloseIdleConnections()
	if _, e := c.Get(s2.URL); e == nil {
		t.Fatal("foreign server accepted")
	}
	w2 := worker(t, d2, s2.URL)
	ws, _ := load(w2)
	cert, _ := tls.X509KeyPair(ws.Cert, ws.Key)
	st, _ := load(d)
	foreign, _ := transport(st.CA, st.Pin, &cert)
	defer foreign.CloseIdleConnections()
	r, e := foreign.Get(s.URL)
	if e == nil {
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Fatal("foreign worker accepted")
		}
	}
	for _, u := range []string{"http://127.0.0.1", "https://8.8.8.8"} {
		if _, e := c.Get(u); e == nil {
			t.Fatal("unsafe request allowed")
		}
	}
}
func TestSameCAPeerNotCoordinator(t *testing.T) {
	d, s := coordinator(t)
	w := worker(t, d, s.URL)
	st, _ := load(d)
	ca, _ := parseCert(st.CA)
	block, _ := pem.Decode(st.CAKey)
	key, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cert := template("peer")
	cert.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	der, e := x509.CreateCertificate(rand.Reader, cert, ca, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	pair, _ := tls.X509KeyPair(certPEM(der), keyPEM(priv))
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("peer reached") }))
	peer.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	peer.StartTLS()
	defer peer.Close()
	c, _ := Client(w)
	defer c.CloseIdleConnections()
	if _, e := c.Get(peer.URL); e == nil {
		t.Fatal("same CA peer accepted")
	}
}
func TestConcurrentSingleUse(t *testing.T) {
	d, s := coordinator(t)
	code := invite(t, d, s.URL)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		dir := filepath.Join(t.TempDir(), "w")
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Join(context.Background(), dir, code) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("successes", wins.Load())
	}
}
func TestProcessHelper(t *testing.T) {
	if os.Getenv("SECURITY_HELPER") != "1" {
		return
	}
	d := os.Getenv("SECURITY_COORD")
	cfg, err := ServerTLS(d)
	if err != nil {
		os.Exit(3)
	}
	s := httptest.NewUnstartedServer(EnrollHandler(d))
	s.TLS = cfg
	s.StartTLS()
	inv := decode(t, os.Getenv("SECURITY_CODE"))
	inv.Endpoint = s.URL
	if e := Join(context.Background(), os.Getenv("SECURITY_DIR"), encode(inv)); e != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestCrossProcessSingleUse(t *testing.T) {
	d, s := coordinator(t)
	code := invite(t, d, s.URL)
	var commands []*exec.Cmd
	for n := 0; n < 4; n++ {
		c := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		c.Env = append(os.Environ(), "SECURITY_HELPER=1", "SECURITY_COORD="+d, "SECURITY_DIR="+filepath.Join(t.TempDir(), "w"), "SECURITY_CODE="+code)
		if e := c.Start(); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
	}
	wins := 0
	for _, c := range commands {
		if c.Wait() == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("process successes", wins)
	}
}
func TestUnsafeStateAndFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink semantics are not portable to Windows")
	}
	d, s := coordinator(t)
	w := worker(t, d, s.URL)
	c, _ := Client(w)
	defer c.CloseIdleConnections()
	if e := os.Chmod(filepath.Join(d, "identity.json"), 0644); e != nil {
		t.Fatal(e)
	}
	if got := status(t, c, s.URL); got != 403 {
		t.Fatal(got)
	}
	if _, e := ServerTLS(d); e == nil {
		t.Fatal("unsafe state accepted")
	}
	target := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(w, target); e != nil {
		t.Fatal(e)
	}
	if _, e := NodeID(target); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestLocalSelfAccess(t *testing.T) {
	d, s := coordinator(t)
	c, e := Client(d)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	if got := status(t, c, s.URL); got != 200 {
		t.Fatal("coordinator self access", got)
	}
	w := filepath.Join(t.TempDir(), "w")
	if e := JoinHost(context.Background(), w, invite(t, d, s.URL), "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	cfg, e := ServerTLS(w)
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewUnstartedServer(Authorize(w, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()
	wc, e := Client(w)
	if e != nil {
		t.Fatal(e)
	}
	defer wc.CloseIdleConnections()
	if got := status(t, wc, srv.URL); got != 200 {
		t.Fatal("worker self access", got)
	}
	if got := status(t, wc, s.URL); got != 200 {
		t.Fatal("worker to coordinator", got)
	}
}

func TestWorkerListenerAndOutboundRevocation(t *testing.T) {
	d, s := coordinator(t)
	w := filepath.Join(t.TempDir(), "w")
	if e := JoinHost(context.Background(), w, invite(t, d, s.URL), "127.0.0.1"); e != nil {
		t.Fatal(e)
	}
	cfg, e := ServerTLS(w)
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(Authorize(w, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte("ok")) })))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()
	c, e := Client(d)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseIdleConnections()
	if got := status(t, c, srv.URL); got != 200 {
		t.Fatal(got)
	}
	other := worker(t, d, s.URL)
	peer, _ := Client(other)
	defer peer.CloseIdleConnections()
	if _, e := peer.Get(srv.URL); e == nil {
		t.Fatal("worker trusted another worker")
	}
	st, _ := load(w)
	ps, _ := load(other)
	pair, _ := tls.X509KeyPair(ps.Cert, ps.Key)
	wc, _ := parseCert(st.Cert)
	attacker, _ := transport(st.CA, digest(wc.Raw), &pair)
	defer attacker.CloseIdleConnections()
	if r, e := attacker.Get(srv.URL); e == nil {
		r.Body.Close()
		t.Fatal("worker listener accepted peer")
	}
	id, _ := NodeID(w)
	if e := Revoke(d, id); e != nil {
		t.Fatal(e)
	}
	if r, e := c.Get(srv.URL); e == nil {
		r.Body.Close()
		t.Fatal("revoked server accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected authorized requests", calls.Load())
	}
}
