package postgres

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCA is a certificate authority that exists only for one test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// serverCert issues a certificate for 127.0.0.1, or for dnsName instead when
// it is set, valid until notAfter.
func (ca testCA) serverCert(t *testing.T, dnsName string, notAfter time.Time) tls.Certificate {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "database"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if dnsName != "" {
		tmpl.DNSNames = []string{dnsName}
	} else {
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// writeFile writes data to a new file in the test's temporary directory and
// returns its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// tlsServer accepts PostgreSQL connections on loopback, answers the client's
// SSLRequest with 'S', and completes a TLS handshake with cert. It speaks no
// further protocol, so a connection that passes TLS fails afterwards.
func tlsServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		wg.Wait()
	})
	config := &tls.Config{Certificates: []tls.Certificate{cert}}
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(guard))
				// An SSLRequest is 8 bytes: its length and the request code.
				if _, err := io.ReadFull(conn, make([]byte, 8)); err != nil {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				_ = tls.Server(conn, config).Handshake()
			})
		}
	})
	return ln.Addr().String()
}

func TestOpenReportsCertificateFailures(t *testing.T) {
	const password = "s3cret-password"
	ca := newTestCA(t, "database CA")
	other := newTestCA(t, "unrelated CA")
	caFile := writeFile(t, "ca.pem", ca.pem)
	otherFile := writeFile(t, "other.pem", other.pem)
	valid := time.Now().Add(time.Hour)

	tests := map[string]struct {
		cert    tls.Certificate
		params  string
		certErr bool
	}{
		"verify-full, CA not in the system store": {
			cert: ca.serverCert(t, "", valid), params: "sslmode=verify-full", certErr: true,
		},
		"verify-full, unrelated CA": {
			cert: ca.serverCert(t, "", valid), params: "sslmode=verify-full&sslrootcert=" + otherFile, certErr: true,
		},
		"verify-full, certificate for another host": {
			cert: ca.serverCert(t, "db.example", valid), params: "sslmode=verify-full&sslrootcert=" + caFile, certErr: true,
		},
		"verify-ca, unrelated CA": {
			cert: ca.serverCert(t, "", valid), params: "sslmode=verify-ca&sslrootcert=" + otherFile, certErr: true,
		},
		"verify-ca, expired certificate": {
			cert: ca.serverCert(t, "", time.Now().Add(-time.Hour)), params: "sslmode=verify-ca&sslrootcert=" + caFile, certErr: true,
		},
		// The handshake succeeds, and the connection fails only afterwards,
		// so the category must not be blamed on the certificate.
		"verify-full, trusted certificate": {
			cert: ca.serverCert(t, "", valid), params: "sslmode=verify-full&sslrootcert=" + caFile,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			addr := tlsServer(t, tc.cert)
			url := "postgres://gateway:" + password + "@" + addr + "/gateway?connect_timeout=5&" + tc.params
			s, err := Open(testContext(t), url, nil)
			if err == nil {
				s.Close()
				t.Fatal("Open() error = nil, want error")
			}
			if got := strings.Contains(err.Error(), "(tls_certificate)"); got != tc.certErr {
				t.Errorf("Open() error = %q, want tls_certificate category: %t", err, tc.certErr)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), addr) {
				t.Errorf("error reveals the URL: %v", err)
			}
		})
	}
}

func TestOpenReportsUnreadableCertificateFile(t *testing.T) {
	const password = "s3cret-password"
	missing := filepath.Join(t.TempDir(), "missing-ca.pem")
	url := "postgres://gateway:" + password + "@127.0.0.1:1/gateway?sslmode=verify-full&sslrootcert=" + missing

	s, err := Open(testContext(t), url, nil)
	if err == nil {
		s.Close()
		t.Fatal("Open() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "cannot read a certificate file named in the database URL") {
		t.Errorf("Open() error = %q, want it to name an unreadable certificate file", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open() error = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
	if strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), password) {
		t.Errorf("error reveals the URL: %v", err)
	}
}
