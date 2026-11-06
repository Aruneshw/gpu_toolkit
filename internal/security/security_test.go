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
