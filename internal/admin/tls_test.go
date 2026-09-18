package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shardpost/internal/adminapi"
)

// testCA issues certificates for the mTLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, _ := x509.ParseCertificate(der)
	ca := &testCA{cert: cert, key: key, dir: t.TempDir()}
	ca.write(t, "ca.crt", "CERTIFICATE", der)

	return ca
}

func (ca *testCA) write(t *testing.T, name, typ string, der []byte) string {
	t.Helper()

	path := filepath.Join(ca.dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// issue writes a leaf key pair as <name>.crt/<name>.key.
func (ca *testCA) issue(t *testing.T, name, cn string, serial int64, usage x509.ExtKeyUsage) (string, string) {
	t.Helper()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	kder, _ := x509.MarshalECPrivateKey(key)

	return ca.write(t, name+".crt", "CERTIFICATE", der), ca.write(t, name+".key", "EC PRIVATE KEY", kder)
}

func TestMutualTLS(t *testing.T) {
	old := reloadInterval
	reloadInterval = 0

	t.Cleanup(func() { reloadInterval = old })

	ca := newTestCA(t)
	caFile := filepath.Join(ca.dir, "ca.crt")
	srvCert, srvKey := ca.issue(t, "server", "shardpost", 10, x509.ExtKeyUsageServerAuth)
	opsCert, opsKey := ca.issue(t, "ops", "OPS", 20, x509.ExtKeyUsageClientAuth)
	strangerCert, strangerKey := ca.issue(t, "stranger", "stranger", 30, x509.ExtKeyUsageClientAuth)

	addr := serveTLS(t, srvCert, srvKey, caFile)
	url := "https://" + addr

	for _, c := range []struct {
		cert, key string
		status    int
	}{{opsCert, opsKey, http.StatusOK}, {strangerCert, strangerKey, http.StatusUnauthorized}} {
		client, err := adminapi.NewTLSClient(url, c.cert, c.key, caFile, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.GetVersionWithResponse(context.Background())
		if err != nil || resp.StatusCode() != c.status {
			t.Fatalf("%s: %v %d", c.cert, err, resp.StatusCode())
		}
	}

	// cert-manager rotates the server certificate in place; the next
	// handshake presents the new one without a restart.
	ca.issue(t, "server", "shardpost", 11, x509.ExtKeyUsageServerAuth)

	if serial := peerSerial(t, addr, opsCert, opsKey, ca); serial != 11 {
		t.Fatalf("rotated certificate not served: serial %d", serial)
	}
}

// peerSerial returns the serial of the certificate the server presents.
func peerSerial(t *testing.T, addr, certFile, keyFile string, ca *testCA) int64 {
	t.Helper()

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	c, err := tls.Dial("tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	return c.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func TestMutualTLSTrustAndRotation(t *testing.T) {
	old := reloadInterval
	reloadInterval = 0

	t.Cleanup(func() { reloadInterval = old })

	ca := newTestCA(t)
	caFile := filepath.Join(ca.dir, "ca.crt")
	srvCert, srvKey := ca.issue(t, "server", "shardpost", 10, x509.ExtKeyUsageServerAuth)

	// A client of another CA, configured identity or not, fails the handshake.
	other := newTestCA(t)
	otherCert, otherKey := other.issue(t, "ops", "ops", 40, x509.ExtKeyUsageClientAuth)

	addr := serveTLS(t, srvCert, srvKey, caFile)
	url := "https://" + addr
	version := func(cert, key string) (int, error) {
		client, err := adminapi.NewTLSClient(url, cert, key, caFile, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.GetVersionWithResponse(context.Background())
		if err != nil {
			return 0, err
		}

		return resp.StatusCode(), nil
	}

	if _, err := version(otherCert, otherKey); err == nil {
		t.Fatal("client of a foreign CA accepted")
	}

	// Rotating the client CA bundle trusts the new CA at once.
	if err := os.WriteFile(caFile, append(pemOf(ca.cert), pemOf(other.cert)...), 0o600); err != nil {
		t.Fatal(err)
	}

	if status, err := version(otherCert, otherKey); err != nil || status != http.StatusOK {
		t.Fatalf("rotated client CA: %v %d", err, status)
	}

	// A broken new server certificate keeps the previous material.
	if err := os.WriteFile(srvCert, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	if serial := peerSerial(t, addr, otherCert, otherKey, ca); serial != 10 {
		t.Fatalf("broken rotation not ignored: serial %d", serial)
	}
}

// serveTLS starts the mTLS listener for the client identity "ops"
// (role diagnose) and returns its address.
func serveTLS(t *testing.T, certFile, keyFile, caFile string) string {
	t.Helper()

	s := newTestServer(t, Options{Clients: map[string][]string{"ops": {RoleDiagnose}}})

	srv, err := s.TLSServer(certFile, keyFile, caFile)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.ServeTLS(ln, "", "") }()

	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String()
}

func pemOf(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}
