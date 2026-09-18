package renew

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// keyBits is the RSA key size the signer requires.
const keyBits = 3072

// pemCertificate is the PEM block type of certificates.
const pemCertificate = "CERTIFICATE"

// Secret keys written (other keys of the Secret are kept).
const (
	KeyCert = "tls.crt"
	KeyKey  = "tls.key"
)

// Signer turns a CSR into a signed certificate (PEM).
type Signer interface {
	Sign(ctx context.Context, csrPEM []byte) ([]byte, error)
}

// Store reads and writes the Secret. Load returns an opaque token that
// Save needs; it makes the write conditional on nobody having changed
// the Secret in between (compare-and-swap).
type Store interface {
	Load(ctx context.Context) (data map[string][]byte, token string, err error)
	Save(ctx context.Context, data map[string][]byte, token string) error
}

// clockSkew tolerates a signer clock slightly ahead of ours (NotBefore of
// a fresh certificate in our future).
const clockSkew = 5 * time.Minute

// ErrShortLived means the signer issued a certificate that lives shorter
// than the renewal window; it was stored, but every run would renew again.
var ErrShortLived = errors.New("issued certificate lives shorter than the renewal window")

// Config is one managed certificate.
type Config struct {
	Subject Subject
	// Root and Issuer are the pinned trust anchors: the RNS root and the
	// client-authentication issuing CA.
	Root, Issuer *x509.Certificate
	// RenewBefore renews when less validity remains.
	RenewBefore time.Duration
	// CAKey, if set, also writes Root (PEM) under this Secret key (the
	// trust anchor a client needs for the LDAP server).
	CAKey  string
	Signer Signer
	Store  Store
	Log    *slog.Logger
	Now    func() time.Time
}

// Result says what Run did.
type Result struct {
	Renewed  bool
	Reason   string
	NotAfter time.Time
}

// Run renews the certificate when needed.
func (c *Config) Run(ctx context.Context) (Result, error) {
	now := c.now()

	data, token, err := c.Store.Load(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("load secret: %w", err)
	}

	reason, notAfter := c.needsRenewal(data, now)
	if reason == "" {
		return Result{Reason: "fresh", NotAfter: notAfter}, nil
	}

	c.log().Info("renewing client certificate", "cn", c.Subject.CommonName(), "reason", reason)

	certPEM, keyPEM, leaf, err := c.issue(ctx, now)
	if err != nil {
		return Result{}, err
	}

	if data == nil {
		data = map[string][]byte{}
	}

	data[KeyCert] = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: c.Issuer.Raw})...)
	data[KeyKey] = keyPEM

	if c.CAKey != "" {
		data[c.CAKey] = c.rootPEM()
	}

	if err := c.Store.Save(ctx, data, token); err != nil {
		return Result{}, fmt.Errorf("save secret: %w", err)
	}

	res := Result{Renewed: true, Reason: reason, NotAfter: leaf.NotAfter}
	if leaf.NotAfter.Sub(now) < c.RenewBefore {
		return res, ErrShortLived
	}

	return res, nil
}

// needsRenewal returns why the stored certificate must be replaced (""
// when it is fine) and its expiry.
func (c *Config) needsRenewal(data map[string][]byte, now time.Time) (string, time.Time) {
	leaf, err := firstCertificate(data[KeyCert])
	if err != nil {
		return "no usable certificate", time.Time{}
	}

	key, err := parseKey(data[KeyKey])
	if err != nil {
		return "no usable private key", leaf.NotAfter
	}

	if err := c.verify(leaf, &key.PublicKey, now); err != nil {
		return err.Error(), leaf.NotAfter
	}

	if leaf.NotAfter.Sub(now) < renewWindow(leaf, c.RenewBefore) {
		return "expires " + leaf.NotAfter.UTC().Format(time.RFC3339), leaf.NotAfter
	}

	if c.CAKey != "" && string(data[c.CAKey]) != string(c.rootPEM()) {
		return "trust anchor changed", leaf.NotAfter
	}

	return "", leaf.NotAfter
}

// renewWindow caps the renewal window at half the certificate's
// lifetime, so a certificate shorter than the configured window is
// renewed once at half time instead of on every run and retry.
func renewWindow(leaf *x509.Certificate, before time.Duration) time.Duration {
	return min(before, leaf.NotAfter.Sub(leaf.NotBefore)/2)
}

// issue creates a key and CSR, has it signed and verifies the result.
func (c *Config) issue(ctx context.Context, now time.Time) (certPEM, keyPEM []byte, leaf *x509.Certificate, err error) {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, nil, nil, err
	}

	subject, err := c.Subject.DER()
	if err != nil {
		return nil, nil, nil, err
	}

	// No extensions: the signer refuses requested SAN, EKU or key usage.
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{RawSubject: subject}, key)
	if err != nil {
		return nil, nil, nil, err
	}

	signed, err := c.Signer.Sign(ctx, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sign: %w", err)
	}

	leaf, err = firstCertificate(signed)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signer answer: %w", err)
	}

	// A fresh certificate may start slightly in our future.
	if err := c.verify(leaf, &key.PublicKey, now.Add(clockSkew)); err != nil {
		return nil, nil, nil, fmt.Errorf("issued certificate: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: leaf.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), leaf, nil
}

// verify checks subject, key, chain to the pinned anchors and clientAuth.
func (c *Config) verify(leaf *x509.Certificate, pub *rsa.PublicKey, now time.Time) error {
	if !c.Subject.Matches(leaf.Subject) {
		return errors.New("subject differs")
	}

	lpub, ok := leaf.PublicKey.(*rsa.PublicKey)
	if !ok || !lpub.Equal(pub) {
		return errors.New("key does not match")
	}

	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(c.Root)
	inter.AddCert(c.Issuer)

	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("chain: %w", err)
	}

	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		return errors.New("EKU is not exactly clientAuth")
	}

	return nil
}

// rootPEM is the pinned root as PEM.
func (c *Config) rootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: c.Root.Raw})
}

func (c *Config) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}

	return slog.Default()
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}

// firstCertificate parses the first PEM certificate.
func firstCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != pemCertificate {
		return nil, errors.New("no PEM certificate")
	}

	return x509.ParseCertificate(block.Bytes)
}

// parseKey parses a PKCS#8 or PKCS#1 RSA key.
func parseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM key")
	}

	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}

	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}

	return rk, nil
}

// LoadCertificate reads one PEM certificate from a file's contents.
func LoadCertificate(data []byte) (*x509.Certificate, error) { return firstCertificate(data) }
