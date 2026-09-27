// Package webpush sends Web Push messages (RFC 8030) as an application
// server: the payload encrypted for the user agent (RFC 8291, aes128gcm)
// and the request signed with the server's VAPID key (RFC 8292).
//
// The push endpoint is a URL a client handed in, so every request goes to
// a host this server knows nothing about. The HTTP client here therefore
// refuses loopback, private, link-local and other internal addresses
// after name resolution, and follows no redirects.
package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Keys are the user agent's keys of a subscription, base64url encoded:
// its P-256 public key and the authentication secret.
type Keys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// recordSize is the rs field of the aes128gcm header (RFC 8188 2.1). The
// payloads here are one short record; the value only has to be larger.
const recordSize = 4096

// Sizes of the RFC 8291 key material.
const (
	publicKeyLen = 65
	authLen      = 16
	saltLen      = 16
)

// ErrBadKeys is returned for subscription keys that cannot be used.
var ErrBadKeys = errors.New("webpush: the subscription keys are not a P-256 key and a 16-byte secret")

// decode reads base64url, with or without padding, and tolerates the
// standard alphabet some clients send.
func decode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(s), "=")

	return base64.RawURLEncoding.DecodeString(s)
}

// b64 is base64url without padding.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// CheckKeys reports whether keys can be used to encrypt.
func CheckKeys(k Keys) error {
	ua, err := decode(k.P256dh)
	if err != nil || len(ua) != publicKeyLen {
		return ErrBadKeys
	}

	if _, err := ecdh.P256().NewPublicKey(ua); err != nil {
		return ErrBadKeys
	}

	auth, err := decode(k.Auth)
	if err != nil || len(auth) != authLen {
		return ErrBadKeys
	}

	return nil
}

// Encrypt encrypts a payload for a subscription with a fresh key pair and
// salt, and returns the aes128gcm body.
func Encrypt(plaintext []byte, k Keys) ([]byte, error) {
	ua, err := decode(k.P256dh)
	if err != nil {
		return nil, ErrBadKeys
	}

	auth, err := decode(k.Auth)
	if err != nil || len(auth) != authLen {
		return nil, ErrBadKeys
	}

	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}

	return encrypt(plaintext, ua, auth, as, salt)
}

// maxPlaintext is the most a push message may carry: one record of
// recordSize less the delimiter and the tag, and within the 4096 octets
// a push service has to accept (RFC 8291 4).
const maxPlaintext = 3993

// ErrTooLarge is returned for a payload one push message cannot carry.
var ErrTooLarge = errors.New("webpush: the payload is too large for one push message")

// encrypt is RFC 8291 3.3 and 3.4 with the random inputs given.
func encrypt(plaintext, uaPublic, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > maxPlaintext {
		return nil, ErrTooLarge
	}

	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, ErrBadKeys
	}

	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}

	asPublic := as.PublicKey().Bytes()

	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)

	ikm, err := hkdf.Key(sha256.New, secret, auth, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}

	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}

	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	header := make([]byte, 0, saltLen+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)

	// One record, and it is the last: the 0x02 delimiter, no padding.
	padded := append(append([]byte(nil), plaintext...), 0x02)

	return gcm.Seal(header, nonce, padded, nil), nil
}

// VAPID is the application server's identity (RFC 8292): the key it signs
// its requests with and a contact the push service can reach.
type VAPID struct {
	key     *ecdsa.PrivateKey
	subject string
}

// LoadVAPID reads a P-256 private key in PEM (PKCS#8 or SEC 1). subject
// is a mailto: or https: URI the push service operator can contact.
func LoadVAPID(pemData []byte, subject string) (*VAPID, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("webpush: the VAPID key is not PEM")
	}

	var key *ecdsa.PrivateKey

	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("webpush: the VAPID key is not an EC key")
		}

		key = ek
	} else if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, fmt.Errorf("webpush: the VAPID key: %w", err)
	}

	if key.Curve != elliptic.P256() {
		return nil, errors.New("webpush: the VAPID key is not on P-256")
	}

	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https:") {
		return nil, errors.New("webpush: the VAPID subject is a mailto: or https: URI")
	}

	return &VAPID{key: key, subject: subject}, nil
}

// PublicKey is the VAPID public key as a client passes it to its push
// service (applicationServerKey): the uncompressed point, base64url.
func (v *VAPID) PublicKey() string {
	pub, err := v.key.PublicKey.ECDH()
	if err != nil {
		return ""
	}

	return b64(pub.Bytes())
}

// vapidTTL is how long a signed token is valid; RFC 8292 2 allows 24h.
const vapidTTL = 12 * time.Hour

// authorization is the Authorization header for a request to endpoint.
func (v *VAPID) authorization(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}

	header := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))

	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(vapidTTL).Unix(),
		"sub": v.subject,
	})
	if err != nil {
		return "", err
	}

	signing := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))

	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", err
	}

	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return "vapid t=" + signing + "." + b64(sig) + ", k=" + v.PublicKey(), nil
}

// Outcome is what a push service made of a message.
type Outcome int

// The outcomes a sender acts on.
const (
	// Delivered: the push service accepted the message.
	Delivered Outcome = iota
	// Gone: the subscription no longer exists (404, 410); drop it.
	Gone
	// Slow: the push service asks for fewer messages (429) or is
	// unavailable; try again later, at RetryAfter if it said.
	Slow
	// Refused: the push service refused this message (another 4xx);
	// sending it again will not help.
	Refused
)

// Result is the outcome of one push, with the delay a push service asked
// for.
type Result struct {
	Outcome    Outcome
	Status     int
	RetryAfter time.Duration
}

// Sender posts push messages.
type Sender struct {
	Client *http.Client
	VAPID  *VAPID // nil: unsigned requests
	TTL    time.Duration
}

// maxResponse is how much of a push service's answer is read.
const maxResponse = 64 << 10

// Send posts a payload to a subscription. keys nil sends it unencrypted,
// which RFC 8620 7.2 allows for a subscription without keys.
func (s *Sender) Send(ctx context.Context, endpoint string, keys *Keys, payload []byte) (Result, error) {
	if len(endpoint) < len("https://") || !strings.EqualFold(endpoint[:len("https://")], "https://") {
		return Result{Outcome: Refused}, errors.New("webpush: the endpoint is not https")
	}

	body, header := payload, http.Header{}
	header.Set("Content-Type", "application/json")

	if keys != nil {
		enc, err := Encrypt(payload, *keys)
		if err != nil {
			return Result{Outcome: Refused}, err
		}

		body = enc

		header.Set("Content-Encoding", "aes128gcm")
		header.Set("Content-Type", "application/octet-stream")
	}

	header.Set("TTL", strconv.Itoa(int(s.TTL/time.Second)))

	if s.VAPID != nil {
		auth, err := s.VAPID.authorization(endpoint, time.Now())
		if err != nil {
			return Result{Outcome: Refused}, err
		}

		header.Set("Authorization", auth)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return Result{Outcome: Refused}, err
	}

	req.Header = header

	resp, err := s.Client.Do(req)
	if err != nil {
		// An endpoint the client may not connect to will not become one
		// by waiting.
		if errors.Is(err, errInternalAddress) || errors.Is(err, errPort) {
			return Result{Outcome: Refused}, err
		}

		return Result{Outcome: Slow}, err
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))

	return classify(resp), nil
}

// classify turns a push service's answer into an outcome.
func classify(resp *http.Response) Result {
	r := Result{Status: resp.StatusCode}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		r.Outcome = Delivered
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		r.Outcome = Gone
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		r.Outcome = Slow
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			r.RetryAfter = time.Duration(secs) * time.Second
		}
	default:
		r.Outcome = Refused
	}

	return r
}

// errInternalAddress refuses a connection to an address inside the
// network, errPort one to a port other than https's.
var (
	errInternalAddress = errors.New("webpush: the push endpoint resolves to an internal address")
	errPort            = errors.New("webpush: the push endpoint is not on port 443")
)

// PushPort is the one port a push endpoint may be on, unless internal
// endpoints are allowed: push services listen on https's port, and a
// client-chosen URL must not make the worker probe any other port of a
// public host.
const PushPort = "443"

// NewClient is an HTTP client for push endpoints. Unless allowInternal is
// set, it refuses to connect to loopback, private, link-local, shared and
// unspecified addresses and to any port but 443 — checked on the address
// it actually dials, after name resolution — and it never follows a
// redirect.
func NewClient(timeout time.Duration, allowInternal bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	if !allowInternal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error { return dialAllowed(address) }
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: timeout,
			MaxIdleConns:        64,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dialAllowed checks an address the client is about to connect to.
func dialAllowed(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}

	if ip := net.ParseIP(host); ip == nil || Internal(ip) {
		return errInternalAddress
	}

	if port != PushPort {
		return errPort
	}

	return nil
}

// internalNets are ranges IsPrivate and its siblings leave out: shared
// address space (RFC 6598), NAT64 (RFC 6052) and 6to4 (RFC 3056), which
// can reach IPv4 space behind them, and the old site-local range.
var internalNets = func() []*net.IPNet {
	var out []*net.IPNet

	for _, c := range []string{"100.64.0.0/10", "0.0.0.0/8", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "fec0::/10"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}

	return out
}()

// Internal reports whether ip is an address a push endpoint must not
// resolve to.
func Internal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}

	for _, n := range internalNets {
		if n.Contains(ip) {
			return true
		}
	}

	return false
}
