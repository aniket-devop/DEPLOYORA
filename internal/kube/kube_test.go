package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type authority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func newAuthority(t *testing.T, name string) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &authority{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (a *authority) issue(t *testing.T, cn string, usage x509.ExtKeyUsage, ips []net.IP, dns []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

type routes map[string]http.HandlerFunc

// serve answers the given paths and fails the test on anything but a GET or a
// request without exactly one client certificate.
func serve(t *testing.T, r routes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("non-GET request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if req.TLS == nil || len(req.TLS.PeerCertificates) != 1 {
			t.Errorf("request without exactly one client certificate")
		}
		h, ok := r[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		h(w, req)
	})
}

func startServer(t *testing.T, ca *authority, h http.Handler) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, "server", x509.ExtKeyUsageServerAuth,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, []string{"localhost"})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.certPEM)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

const configTemplate = `apiVersion: v1
kind: Config
current-context: c
preferences: {}
contexts:
- name: c
  context:
    cluster: c
    user: u
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
%s
users:
- name: u
  user:
    client-certificate-data: %s
    client-key-data: %s
%s
`

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func configYAML(server, ca, cert, key, clusterExtra, userExtra string) string {
	return fmt.Sprintf(configTemplate, server, ca, clusterExtra, cert, key, userExtra)
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newClient connects to srv with a client certificate issued by clientCA and
// trusting trustCA as the server's authority.
func newClientWith(t *testing.T, srv *httptest.Server, trustCA, clientCA *authority, opts Options) *Client {
	t.Helper()
	certPEM, keyPEM := clientCA.issue(t, "client", x509.ExtKeyUsageClientAuth, nil, nil)
	path := writeFile(t, configYAML(srv.URL, b64(trustCA.certPEM), b64(certPEM), b64(keyPEM), "", ""))
	c, err := New(path, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func newClient(t *testing.T, srv *httptest.Server, ca *authority, opts Options) *Client {
	t.Helper()
	return newClientWith(t, srv, ca, ca, opts)
}

func status(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	} `json:"items"`
}

func TestReadyAndGet(t *testing.T) {
	ca := newAuthority(t, "ca")
	srv := startServer(t, ca, serve(t, routes{
		"/readyz": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") },
		"/api/v1/namespaces/ns/pods": func(w http.ResponseWriter, r *http.Request) {
			status(w, 200, `{"items":[{"metadata":{"name":"p1"}},{"metadata":{"name":"p2"}}]}`)
		},
	}))
	c := newClient(t, srv, ca, Options{})
	ctx := context.Background()

	if err := c.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var pods podList
	if err := c.Get(ctx, "/api/v1/namespaces/ns/pods", &pods); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(pods.Items) != 2 || pods.Items[1].Metadata.Name != "p2" {
		t.Errorf("got %+v", pods)
	}
	if err := c.Get(ctx, "/readyz", nil); err != nil {
		t.Errorf("Get with a nil target: %v", err)
	}
}

func TestNotReady(t *testing.T) {
	ca := newAuthority(t, "ca")
	srv := startServer(t, ca, serve(t, routes{
		"/readyz": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "[+]ping ok\n[-]etcd failed: reason withheld\nreadyz check failed", http.StatusInternalServerError)
		},
	}))
	err := newClient(t, srv, ca, Options{}).Ready(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 || !strings.Contains(se.Message, "etcd failed") {
		t.Fatalf("got %v, want a 500 StatusError that names etcd", err)
	}
}

func TestStatusErrors(t *testing.T) {
	ca := newAuthority(t, "ca")
	srv := startServer(t, ca, serve(t, routes{
		"/notfound": func(w http.ResponseWriter, r *http.Request) {
			status(w, 404, `{"kind":"Status","code":404,"reason":"NotFound","message":"pods \"x\" not found"}`)
		},
		"/forbidden": func(w http.ResponseWriter, r *http.Request) {
			status(w, 403, `{"kind":"Status","code":403,"reason":"Forbidden","message":"nope"}`)
		},
		"/unauthorized": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		},
	}))
	c := newClient(t, srv, ca, Options{})
	ctx := context.Background()

	err := c.Get(ctx, "/notfound", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Reason != "NotFound" || !strings.Contains(se.Message, "not found") || !IsNotFound(err) {
		t.Errorf("404: got %v", err)
	}
	if err := c.Get(ctx, "/forbidden", nil); !IsForbidden(err) || IsNotFound(err) {
		t.Errorf("403: got %v", err)
	}
	err = c.Get(ctx, "/unauthorized", nil)
	if !IsUnauthorized(err) || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("401: got %v", err)
	}
	if err := c.Get(ctx, "/missing", nil); !IsNotFound(err) {
		t.Errorf("unknown path: got %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	ca := newAuthority(t, "ca")
	var readyzHits atomic.Int32
	srv := startServer(t, ca, serve(t, routes{
		"/redir": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/readyz", http.StatusFound) },
		"/readyz": func(w http.ResponseWriter, r *http.Request) {
			readyzHits.Add(1)
			io.WriteString(w, "ok")
		},
	}))
	err := newClient(t, srv, ca, Options{}).Get(context.Background(), "/redir", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Fatalf("got %v, want a 302 StatusError", err)
	}
	if n := readyzHits.Load(); n != 0 {
		t.Errorf("the redirect was followed (%d hits)", n)
	}
}

func TestResponseTooLarge(t *testing.T) {
	ca := newAuthority(t, "ca")
	srv := startServer(t, ca, serve(t, routes{
		"/big": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", 1000)) },
	}))
	err := newClient(t, srv, ca, Options{MaxResponse: 100}).Get(context.Background(), "/big", nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

func TestContextDeadline(t *testing.T) {
	ca := newAuthority(t, "ca")
	srv := startServer(t, ca, serve(t, routes{
		"/slow": func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := newClient(t, srv, ca, Options{}).Get(ctx, "/slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want context.DeadlineExceeded", err)
	}
}

func TestInvalidPathsNeverReachTheServer(t *testing.T) {
	ca := newAuthority(t, "ca")
	var hits atomic.Int32
	srv := startServer(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "ok")
	}))
	c := newClient(t, srv, ca, Options{})
	for _, p := range []string{"", "readyz", "//evil.example/x", "https://example.com/x", "/a b", "/x#y", `/a\b`} {
		if err := c.Get(context.Background(), p, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("path %q: got %v, want ErrInvalid", p, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d request(s) reached the server", n)
	}
}

func TestClientCertificateFromAnotherAuthorityIsRejected(t *testing.T) {
	ca, other := newAuthority(t, "ca"), newAuthority(t, "other")
	srv := startServer(t, ca, serve(t, routes{
		"/readyz": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") },
	}))
	err := newClientWith(t, srv, ca, other, Options{}).Ready(context.Background())
	var se *StatusError
	if err == nil || errors.As(err, &se) {
		t.Errorf("got %v, want a TLS error, not an answer", err)
	}
}

func TestServerCertificateFromAnotherAuthorityIsRejected(t *testing.T) {
	ca, other := newAuthority(t, "ca"), newAuthority(t, "other")
	srv := startServer(t, ca, serve(t, routes{
		"/readyz": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") },
	}))
	err := newClientWith(t, srv, other, ca, Options{}).Ready(context.Background())
	var se *StatusError
	if err == nil || errors.As(err, &se) {
		t.Errorf("got %v, want a TLS error, not an answer", err)
	}
}

func TestAcceptsLoopbackServers(t *testing.T) {
	ca := newAuthority(t, "ca")
	certPEM, keyPEM := ca.issue(t, "client", x509.ExtKeyUsageClientAuth, nil, nil)
	for _, server := range []string{"https://127.0.0.1:6443", "https://localhost:6443", "https://[::1]:6443", "https://127.0.0.1:6443/"} {
		path := writeFile(t, configYAML(server, b64(ca.certPEM), b64(certPEM), b64(keyPEM), "", ""))
		c, err := New(path, Options{})
		if err != nil {
			t.Errorf("%s: %v", server, err)
			continue
		}
		c.Close()
	}
}

func TestKubeconfigRejections(t *testing.T) {
	ca := newAuthority(t, "ca")
	certPEM, keyPEM := ca.issue(t, "client", x509.ExtKeyUsageClientAuth, nil, nil)
	_, otherKeyPEM := ca.issue(t, "other", x509.ExtKeyUsageClientAuth, nil, nil)
	caB, certB, keyB, otherKeyB := b64(ca.certPEM), b64(certPEM), b64(keyPEM), b64(otherKeyPEM)
	const good = "https://127.0.0.1:6443"

	cases := []struct {
		name string
		yaml string
	}{
		{"http scheme", configYAML("http://127.0.0.1:6443", caB, certB, keyB, "", "")},
		{"non-loopback address", configYAML("https://192.0.2.1:6443", caB, certB, keyB, "", "")},
		{"hostname", configYAML("https://example.com:6443", caB, certB, keyB, "", "")},
		{"credentials in the URL", configYAML("https://user:secretpw@127.0.0.1:6443", caB, certB, keyB, "", "")},
		{"path in the URL", configYAML("https://127.0.0.1:6443/proxy", caB, certB, keyB, "", "")},
		{"query in the URL", configYAML("https://127.0.0.1:6443/?a=b", caB, certB, keyB, "", "")},
		{"empty server", configYAML("", caB, certB, keyB, "", "")},
		{"insecure skip verify", configYAML(good, caB, certB, keyB, "    insecure-skip-tls-verify: true\n", "")},
		{"proxy url", configYAML(good, caB, certB, keyB, "    proxy-url: http://proxy.invalid:3128\n", "")},
		{"tls server name", configYAML(good, caB, certB, keyB, "    tls-server-name: other\n", "")},
		{"CA file", configYAML(good, caB, certB, keyB, "    certificate-authority: /etc/ca.crt\n", "")},
		{"token", configYAML(good, caB, certB, keyB, "", "    token: abc\n")},
		{"token file", configYAML(good, caB, certB, keyB, "", "    tokenFile: /t\n")},
		{"exec plugin", configYAML(good, caB, certB, keyB, "", "    exec:\n      command: foo\n")},
		{"auth provider", configYAML(good, caB, certB, keyB, "", "    auth-provider:\n      name: gcp\n")},
		{"username and password", configYAML(good, caB, certB, keyB, "", "    username: u\n    password: p\n")},
		{"impersonation", configYAML(good, caB, certB, keyB, "", "    as: admin\n")},
		{"client certificate file", configYAML(good, caB, certB, keyB, "", "    client-certificate: /c\n")},
		{"missing CA data", configYAML(good, "", certB, keyB, "", "")},
		{"missing client certificate", configYAML(good, caB, "", keyB, "", "")},
		{"missing client key", configYAML(good, caB, certB, "", "", "")},
		{"bad base64", configYAML(good, "!!!!", certB, keyB, "", "")},
		{"CA data is not a certificate", configYAML(good, b64([]byte("hello")), certB, keyB, "", "")},
		{"certificate and key do not match", configYAML(good, caB, certB, otherKeyB, "", "")},
		{"two clusters", fmt.Sprintf("clusters:\n- name: a\n  cluster: {server: \"%s\", certificate-authority-data: %s}\n- name: b\n  cluster: {server: \"%s\", certificate-authority-data: %s}\nusers:\n- name: u\n  user: {client-certificate-data: %s, client-key-data: %s}\n", good, caB, good, caB, certB, keyB)},
		{"no user", fmt.Sprintf("clusters:\n- name: a\n  cluster: {server: \"%s\", certificate-authority-data: %s}\nusers: []\n", good, caB)},
		{"not YAML", "a: [1, 2\n"},
		{"empty file", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(writeFile(t, tc.yaml), Options{})
			if c != nil {
				c.Close()
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
			for _, secret := range []string{caB, certB, keyB, otherKeyB, "secretpw"} {
				if len(secret) > 8 && strings.Contains(err.Error(), secret) {
					t.Errorf("the error leaks secret material: %v", err)
				}
			}
		})
	}
}

func TestNewRejectsRelativeAndMissingPaths(t *testing.T) {
	if _, err := New("kubeconfig", Options{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("relative path: got %v, want ErrInvalid", err)
	}
	if _, err := New(filepath.Join(t.TempDir(), "nope"), Options{}); err == nil {
		t.Error("missing file: want an error")
	}
}
