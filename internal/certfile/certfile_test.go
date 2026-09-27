package certfile

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair writes a self-signed certificate with the given serial into
// dir and returns the file names.
func writePair(t *testing.T, dir string, serial int64) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "jmap.example.org"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	cf, kf := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}

	return cf, kf
}

// serialOf is the serial of the certificate the source presents.
func serialOf(t *testing.T, s *Source) int64 {
	t.Helper()

	c, err := s.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	return leaf.SerialNumber.Int64()
}

// TestReloadsRenewedFiles: a pair rewritten in place is picked up; a
// broken one is not, and the loaded certificate stays.
func TestReloadsRenewedFiles(t *testing.T) {
	dir := t.TempDir()
	cf, kf := writePair(t, dir, 1)

	s, err := New(cf, kf, time.Nanosecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := serialOf(t, s); got != 1 {
		t.Fatalf("serial %d, want 1", got)
	}

	writePair(t, dir, 2)

	later := time.Now().Add(time.Second)
	_ = os.Chtimes(cf, later, later)

	if got := serialOf(t, s); got != 2 {
		t.Fatalf("after renewal serial %d, want 2", got)
	}

	if err := os.WriteFile(cf, []byte("half written"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := serialOf(t, s); got != 2 {
		t.Fatalf("after a broken renewal serial %d, want the loaded 2", got)
	}
}

// TestFollowsSecretSymlinkSwap: Kubernetes updates a mounted Secret by
// pointing a symlink at a new directory.
func TestFollowsSecretSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	writePair(t, filepath.Join(root, "a"), 10)
	writePair(t, filepath.Join(root, "b"), 11)

	data := filepath.Join(root, "..data")
	if err := os.Symlink("a", data); err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", n), filepath.Join(root, n)); err != nil {
			t.Fatal(err)
		}
	}

	s, err := New(filepath.Join(root, "tls.crt"), filepath.Join(root, "tls.key"), time.Nanosecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := serialOf(t, s); got != 10 {
		t.Fatalf("serial %d, want 10", got)
	}

	tmp := filepath.Join(root, "..data_tmp")
	if err := os.Symlink("b", tmp); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, data); err != nil {
		t.Fatal(err)
	}

	later := time.Now().Add(time.Second)
	_ = os.Chtimes(filepath.Join(root, "b", "tls.crt"), later, later)

	if got := serialOf(t, s); got != 11 {
		t.Fatalf("after the swap serial %d, want 11", got)
	}
}

// TestNoIntervalNeverReloads, and the first load has to succeed.
func TestNoIntervalNeverReloads(t *testing.T) {
	dir := t.TempDir()
	cf, kf := writePair(t, dir, 1)

	s, err := New(cf, kf, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	writePair(t, dir, 2)

	later := time.Now().Add(time.Second)
	_ = os.Chtimes(cf, later, later)

	if got := serialOf(t, s); got != 1 {
		t.Fatalf("serial %d without an interval, want 1", got)
	}

	if _, err := New(filepath.Join(dir, "missing.crt"), kf, time.Minute, nil); err == nil {
		t.Fatal("a missing certificate was accepted at startup")
	}
}
