package integration

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// DefaultHTTPClient returns the http.Client used by vault integration
// calls when no custom client is configured: 10s timeout, system CA
// trust. Exposed so callers (renewer, approle login) can opt in to the
// same defaults.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// NewHTTPClient returns an *http.Client suitable for vault HTTP calls.
// When caCertPath is empty, the returned client uses Go's default TLS
// trust (the system CA bundle). When caCertPath points at a PEM file,
// the returned client trusts only certificates chaining to that bundle
// — matching vault's own VAULT_CACERT semantics. Operators who need to
// keep public-CA trust alongside a private CA must concatenate the
// bundles themselves.
func NewHTTPClient(caCertPath string) (*http.Client, error) {
	client := DefaultHTTPClient()
	if caCertPath == "" {
		return client, nil
	}
	pem, err := os.ReadFile(caCertPath) //nolint:gosec // G304: operator-provided path
	if err != nil {
		return nil, fmt.Errorf("vault ca_cert: read %s: %w", caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("vault ca_cert: no valid PEM certificates in %s", caCertPath)
	}
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	return client, nil
}
