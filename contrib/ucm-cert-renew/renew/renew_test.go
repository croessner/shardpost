package renew

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"
)

const testSubject = "C=DE,DC=de,DC=roessner-net,ST=Hessen,L=Alsfeld,O=R.N.S.,OU=Mail,CN=shardpost-quota.srvint.net,emailAddress=c@example.org"

// testPKI is a root and a client issuing CA like the UCM hierarchy.
type testPKI struct {
	root, issuer       *x509.Certificate
	rootKey, issuerKey *ecdsa.PrivateKey
	serial             int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()

	p := &testPKI{serial: 10}
	p.rootKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.issuerKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "RNS Internal Root CA 2026"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	p.root = mustCert(t, root, root, &p.rootKey.PublicKey, p.rootKey)

	issuer := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "RNS Client Authentication Issuing CA 01"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	p.issuer = mustCert(t, issuer, p.root, &p.issuerKey.PublicKey, p.rootKey)

	return p
}

func mustCert(t *testing.T, tmpl, parent *x509.Certificate, pub any, key any) *x509.Certificate {
	t.Helper()

	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
	if err != nil {
		t.Fatal(err)
	}

	c, _ := x509.ParseCertificate(der)

	return c
}

// sign issues a leaf for the CSR like the UCM signer (clientAuth only),
// with the given lifetime.
func (p *testPKI) sign(t *testing.T, csrPEM []byte, lifetime time.Duration, eku x509.ExtKeyUsage) []byte {
	t.Helper()

	block, _ := pem.Decode(csrPEM)

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil || len(csr.Extensions) != 0 {
		t.Fatalf("csr: %v", err)
	}

	p.serial++
	leaf := &x509.Certificate{SerialNumber: big.NewInt(p.serial), RawSubject: csr.RawSubject,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(lifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku}}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mustCert(t, leaf, p.issuer, csr.PublicKey, p.issuerKey).Raw})
}

type fakeSigner struct {
	t        *testing.T
	pki      *testPKI
	lifetime time.Duration
	eku      x509.ExtKeyUsage
	calls    int
	err      error
}

type failingStore struct{ memStore }

func (f *failingStore) Save(context.Context, map[string][]byte, string) error {
	return errors.New("conflict")
}

func (f *fakeSigner) Sign(_ context.Context, csr []byte) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}

	return f.pki.sign(f.t, csr, f.lifetime, f.eku), nil
}

type memStore struct {
	data  map[string][]byte
	saves int
}

func (m *memStore) Load(context.Context) (map[string][]byte, string, error) {
	cp := map[string][]byte{}
	for k, v := range m.data {
		cp[k] = v
	}

	return cp, "v1", nil
}

func (m *memStore) Save(_ context.Context, data map[string][]byte, version string) error {
	if version != "v1" {
		return errors.New("bad version")
	}

	m.data, m.saves = data, m.saves+1

	return nil
}

func testConfig(t *testing.T) (*Config, *fakeSigner, *memStore) {
	t.Helper()

	pki := newTestPKI(t)

	subject, err := ParseSubject(testSubject)
	if err != nil {
		t.Fatal(err)
	}

	signer := &fakeSigner{t: t, pki: pki, lifetime: 90 * 24 * time.Hour, eku: x509.ExtKeyUsageClientAuth}
	store := &memStore{data: map[string][]byte{"other": []byte("kept")}}

	return &Config{Subject: subject, Root: pki.root, Issuer: pki.issuer, RenewBefore: 30 * 24 * time.Hour,
		CAKey: "ca.crt", Signer: signer, Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, signer, store
}

// checkLayout verifies the written Secret: kept keys, chain, PKCS#8 key.
func checkLayout(t *testing.T, data map[string][]byte) {
	t.Helper()

	if string(data["other"]) != "kept" || strings.Count(string(data[KeyCert]), "BEGIN CERTIFICATE") != 2 ||
		!strings.Contains(string(data[KeyKey]), "BEGIN PRIVATE KEY") || !strings.Contains(string(data["ca.crt"]), "BEGIN CERTIFICATE") {
		t.Fatal("secret layout")
	}

	if key, err := parseKey(data[KeyKey]); err != nil || key.N.BitLen() != keyBits {
		t.Fatalf("key: %v", err)
	}
}

func TestRenewLifecycle(t *testing.T) {
	cfg, signer, store := testConfig(t)

	res, err := cfg.Run(context.Background())
	if err != nil || !res.Renewed || res.Reason != "no usable certificate" {
		t.Fatalf("first run: %+v %v", res, err)
	}

	checkLayout(t, store.data)

	res, err = cfg.Run(context.Background())
	if err != nil || res.Renewed || res.Reason != "fresh" || signer.calls != 1 {
		t.Fatalf("second run: %+v %v", res, err)
	}

	// 70 days later less than 30 remain.
	cfg.Now = func() time.Time { return time.Now().Add(70 * 24 * time.Hour) }
	signer.lifetime = 170 * 24 * time.Hour

	if res, err = cfg.Run(context.Background()); err != nil || !res.Renewed || !strings.HasPrefix(res.Reason, "expires ") {
		t.Fatalf("expiring: %+v %v", res, err)
	}
}

func TestRenewRejects(t *testing.T) {
	cfg, signer, store := testConfig(t)

	signer.eku = x509.ExtKeyUsageServerAuth

	if _, err := cfg.Run(context.Background()); err == nil || store.saves != 0 {
		t.Fatalf("serverAuth certificate stored: %v", err)
	}

	signer.eku, signer.err = x509.ExtKeyUsageClientAuth, errors.New("refused")
	if _, err := cfg.Run(context.Background()); err == nil || store.saves != 0 {
		t.Fatal("signer failure stored")
	}

	// A certificate from a foreign CA or for another subject is replaced.
	signer.err = nil
	if _, err := cfg.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	other, _ := ParseSubject(strings.Replace(testSubject, "shardpost-quota", "other", 1))
	cfg.Subject = other

	if res, err := cfg.Run(context.Background()); err != nil || !res.Renewed || res.Reason != "subject differs" {
		t.Fatalf("foreign subject: %+v %v", res, err)
	}

	foreign := newTestPKI(t)
	cfg.Root, cfg.Issuer, signer.pki = foreign.root, foreign.issuer, foreign

	if res, err := cfg.Run(context.Background()); err != nil || !res.Renewed || !strings.HasPrefix(res.Reason, "chain: ") {
		t.Fatalf("foreign chain: %+v %v", res, err)
	}
}

func TestSubject(t *testing.T) {
	s, err := ParseSubject(testSubject)
	if err != nil || s.CommonName() != "shardpost-quota.srvint.net" {
		t.Fatalf("%v %v", s, err)
	}

	der, _ := s.DER()

	var name pkix.RDNSequence
	if _, err := asn1.Unmarshal(der, &name); err != nil {
		t.Fatal(err)
	}

	// The string types the signer (Python cryptography) compares with.
	var raw []asn1.RawValue
	if _, err := asn1.Unmarshal(der, &raw); err != nil {
		t.Fatal(err)
	}

	tags := []int{}

	for _, rdn := range raw {
		var set []struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue
		}
		if _, err := asn1.UnmarshalWithParams(rdn.FullBytes, &set, "set"); err != nil {
			t.Fatal(err)
		}

		tags = append(tags, set[0].Value.Tag)
	}

	want := []int{asn1.TagPrintableString, asn1.TagIA5String, asn1.TagIA5String, asn1.TagUTF8String, asn1.TagUTF8String,
		asn1.TagUTF8String, asn1.TagUTF8String, asn1.TagUTF8String, asn1.TagIA5String}
	if fmt.Sprint(tags) != fmt.Sprint(want) {
		t.Fatalf("string types %v, want %v", tags, want)
	}

	var parsed pkix.Name
	parsed.FillFromRDNSequence(&name)

	if !s.Matches(parsed) || len(parsed.Names) != 9 {
		t.Fatalf("round trip: %v", parsed)
	}

	for _, bad := range []string{"", "CN=", "X=1,CN=a", "OU=Mail", "CN=a+b", "CN=a;b"} {
		if _, err := ParseSubject(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRenewShortLivedAndExpired(t *testing.T) {
	cfg, signer, store := testConfig(t)

	// Shorter than the renewal window: stored, but reported.
	signer.lifetime = 10 * 24 * time.Hour

	if res, err := cfg.Run(context.Background()); !errors.Is(err, ErrShortLived) || !res.Renewed || store.saves != 1 {
		t.Fatalf("short-lived: %+v %v", res, err)
	}

	// A job retry right after must not issue again (half-time window).
	if res, err := cfg.Run(context.Background()); err != nil || res.Renewed || signer.calls != 1 {
		t.Fatalf("retry after short-lived: %+v %v calls=%d", res, err, signer.calls)
	}

	signer.lifetime = 90 * 24 * time.Hour
	cfg.Now = func() time.Time { return time.Now().Add(20 * 24 * time.Hour) }

	if res, err := cfg.Run(context.Background()); err != nil || !res.Renewed || !strings.HasPrefix(res.Reason, "chain: ") {
		t.Fatalf("expired: %+v %v", res, err)
	}
}

func TestRenewBrokenSecret(t *testing.T) {
	cfg, _, store := testConfig(t)

	if _, err := cfg.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	delete(store.data, KeyKey)

	if res, err := cfg.Run(context.Background()); err != nil || res.Reason != "no usable private key" {
		t.Fatalf("missing key: %+v %v", res, err)
	}

	store.data["ca.crt"] = []byte("old anchor")

	if res, err := cfg.Run(context.Background()); err != nil || res.Reason != "trust anchor changed" {
		t.Fatalf("anchor: %+v %v", res, err)
	}

	// A failing write is an error; nothing is half written.
	cfg.Store = &failingStore{}

	if _, err := cfg.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "save secret") {
		t.Fatalf("save failure: %v", err)
	}
}

func TestSignerClockAhead(t *testing.T) {
	cfg, signer, _ := testConfig(t)

	// The signer's clock is two minutes ahead: NotBefore is in our future.
	cfg.Now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
	signer.lifetime = 90 * 24 * time.Hour

	if res, err := cfg.Run(context.Background()); err != nil || !res.Renewed {
		t.Fatalf("clock skew: %+v %v", res, err)
	}
}

func TestSubjectCharset(t *testing.T) {
	for _, bad := range []string{"C=DÄ,CN=a", "DC=ä,CN=a", "emailAddress=ä@x,CN=a", "C=D*,CN=a"} {
		if _, err := ParseSubject(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	if _, err := ParseSubject("L=Alsfeld,O=Rößner,CN=a"); err != nil {
		t.Fatalf("UTF-8 in a UTF8String: %v", err)
	}
}
