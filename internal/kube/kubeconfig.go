package kube

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const maxKubeconfigSize = 1 << 20

type kubeconfig struct {
	Clusters []struct {
		Name    string      `yaml:"name"`
		Cluster clusterSpec `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string   `yaml:"name"`
		User userSpec `yaml:"user"`
	} `yaml:"users"`
}

type clusterSpec struct {
	Server                string `yaml:"server"`
	CAData                string `yaml:"certificate-authority-data"`
	CAFile                string `yaml:"certificate-authority"`
	InsecureSkipTLSVerify bool   `yaml:"insecure-skip-tls-verify"`
	TLSServerName         string `yaml:"tls-server-name"`
	ProxyURL              string `yaml:"proxy-url"`
}

type userSpec struct {
	CertData     string `yaml:"client-certificate-data"`
	KeyData      string `yaml:"client-key-data"`
	CertFile     string `yaml:"client-certificate"`
	KeyFile      string `yaml:"client-key"`
	Token        string `yaml:"token"`
	TokenFile    string `yaml:"tokenFile"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	Exec         any    `yaml:"exec"`
	AuthProvider any    `yaml:"auth-provider"`
	As           string `yaml:"as"`
}

// New builds a client from the kubeconfig at kubeconfigPath (absolute). It
// accepts only the shape Kind writes: one cluster on a loopback address, one
// user with an embedded client certificate and key. Anything that would change
// how the client authenticates or where it connects is rejected. New does not
// connect to anything.
func New(kubeconfigPath string, opts Options) (*Client, error) {
	if !filepath.IsAbs(kubeconfigPath) {
		return nil, fmt.Errorf("%w: kubeconfig path %q must be absolute", ErrInvalid, kubeconfigPath)
	}
	info, err := os.Stat(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	if info.Size() > maxKubeconfigSize {
		return nil, fmt.Errorf("%w: kubeconfig is larger than %d bytes", ErrInvalid, maxKubeconfigSize)
	}
	data, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}

	var cfg kubeconfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%w: cannot parse the kubeconfig: %v", ErrInvalid, err)
	}
	if len(cfg.Clusters) != 1 || len(cfg.Users) != 1 {
		return nil, fmt.Errorf("%w: the kubeconfig must hold exactly one cluster and one user, found %d and %d",
			ErrInvalid, len(cfg.Clusters), len(cfg.Users))
	}
	cl, us := cfg.Clusters[0].Cluster, cfg.Users[0].User

	base, err := checkServer(cl.Server)
	if err != nil {
		return nil, err
	}
	if bad := unsupportedCluster(cl); len(bad) > 0 {
		return nil, fmt.Errorf("%w: unsupported cluster settings: %s", ErrInvalid, strings.Join(bad, ", "))
	}
	if bad := unsupportedUser(us); len(bad) > 0 {
		return nil, fmt.Errorf("%w: unsupported credentials: %s", ErrInvalid, strings.Join(bad, ", "))
	}
	tlsCfg, err := buildTLS(cl, us)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxResponse := opts.MaxResponse
	if maxResponse <= 0 {
		maxResponse = defaultMaxResponse
	}
	tr := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		base:      base,
		transport: tr,
		http: &http.Client{
			Transport:     tr,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		maxResponse: maxResponse,
	}, nil
}

func checkServer(raw string) (string, error) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil {
		return "", fmt.Errorf("%w: the server is missing or not a valid URL", ErrInvalid)
	}
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("%w: the server must use https", ErrInvalid)
	case u.User != nil:
		return "", fmt.Errorf("%w: the server URL must not carry credentials", ErrInvalid)
	case u.Opaque != "" || u.Hostname() == "":
		return "", fmt.Errorf("%w: the server URL has no host", ErrInvalid)
	case !isLoopback(u.Hostname()):
		return "", fmt.Errorf("%w: the server host %q is not a loopback address; only the cluster a run created on this machine is allowed", ErrInvalid, u.Hostname())
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%w: the server URL must not have a path, query or fragment", ErrInvalid)
	}
	return "https://" + u.Host, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func unsupportedCluster(c clusterSpec) []string {
	var bad []string
	if c.CAFile != "" {
		bad = append(bad, "certificate-authority (use certificate-authority-data)")
	}
	if c.InsecureSkipTLSVerify {
		bad = append(bad, "insecure-skip-tls-verify")
	}
	if c.TLSServerName != "" {
		bad = append(bad, "tls-server-name")
	}
	if c.ProxyURL != "" {
		bad = append(bad, "proxy-url")
	}
	return bad
}

func unsupportedUser(u userSpec) []string {
	var bad []string
	add := func(set bool, name string) {
		if set {
			bad = append(bad, name)
		}
	}
	add(u.CertFile != "", "client-certificate (use client-certificate-data)")
	add(u.KeyFile != "", "client-key (use client-key-data)")
	add(u.Token != "", "token")
	add(u.TokenFile != "", "tokenFile")
	add(u.Username != "" || u.Password != "", "username or password")
	add(u.Exec != nil, "exec")
	add(u.AuthProvider != nil, "auth-provider")
	add(u.As != "", "as")
	return bad
}

func buildTLS(c clusterSpec, u userSpec) (*tls.Config, error) {
	caPEM, err := decodeData("certificate-authority-data", c.CAData)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%w: certificate-authority-data holds no certificate", ErrInvalid)
	}
	certPEM, err := decodeData("client-certificate-data", u.CertData)
	if err != nil {
		return nil, err
	}
	keyPEM, err := decodeData("client-key-data", u.KeyData)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: the client certificate and key do not form a valid pair: %v", ErrInvalid, err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{pair}}, nil
}

// decodeData decodes a base64 field. Errors name the field and never include its content.
func decodeData(field, s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	if s == "" {
		return nil, fmt.Errorf("%w: %s is missing", ErrInvalid, field)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid base64", ErrInvalid, field)
	}
	return b, nil
}
