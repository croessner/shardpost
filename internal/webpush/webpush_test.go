package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()

	b, err := decode(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// TestEncryptMatchesRFC8291: the example of RFC 8291 appendix A, byte for
// byte.
func TestEncryptMatchesRFC8291(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustDecode(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := encrypt(
		mustDecode(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24"),
		mustDecode(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustDecode(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		asPriv,
		mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}

	header := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	ciphertext := "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ"

	if len(got) != 86+len(mustDecode(t, ciphertext)) || b64(got[:86]) != header || b64(got[86:]) != ciphertext {
		t.Errorf("encrypted to %s, want header %s and ciphertext %s", b64(got), header, ciphertext)
	}
}

// testVAPID is a fresh VAPID identity.
func testVAPID(t *testing.T) *VAPID {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	v, err := LoadVAPID(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "mailto:ops@example.org")
	if err != nil {
		t.Fatal(err)
	}

	return v
}

// TestVAPIDToken: the token verifies with the public key it names and
// carries the audience of the endpoint.
func TestVAPIDToken(t *testing.T) {
	v := testVAPID(t)

	auth, err := v.authorization("https://push.example.net/wpush/abc?x=1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	token, k, ok := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ", k=")
	if !ok || k != v.PublicKey() {
		t.Fatalf("authorization is %q", auth)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}

	var claims map[string]any
	if err := json.Unmarshal(mustDecode(t, parts[1]), &claims); err != nil || claims["aud"] != "https://push.example.net" {
		t.Errorf("claims %v", claims)
	}

	sig := mustDecode(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))

	if !ecdsa.Verify(&v.key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("the signature does not verify")
	}
}

// TestInternalAddresses: what the client refuses to connect to.
func TestInternalAddresses(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.5.4", "169.254.169.254", "100.64.1.1",
		"0.0.0.0", "::1", "fd00::1", "fe80::1", "64:ff9b::a00:1", "2002:a00:1::1", "fec0::1", "::ffff:127.0.0.1"} {
		if !Internal(net.ParseIP(s)) {
			t.Errorf("%s is not refused", s)
		}
	}

	for _, s := range []string{"8.8.8.8", "2001:4860:4860::8888"} {
		if Internal(net.ParseIP(s)) {
			t.Errorf("%s is refused", s)
		}
	}
}

// TestSendRefusesALoopbackEndpoint: the guard is on the dialled address.
func TestSendRefusesALoopbackEndpoint(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	s := &Sender{Client: NewClient(2*time.Second, false), TTL: time.Minute}

	res, err := s.Send(context.Background(), srv.URL, nil, []byte(`{}`))
	if err == nil {
		t.Fatal("a loopback endpoint was reached")
	}

	if res.Outcome != Refused {
		t.Errorf("a refused endpoint is %v, not Refused: it would be retried", res.Outcome)
	}
}

// TestDialAllowed: a public address on 443 only.
func TestDialAllowed(t *testing.T) {
	for addr, want := range map[string]error{
		"93.184.216.34:443":  nil,
		"93.184.216.34:8443": errPort,
		"[2606:2800::1]:22":  errPort,
		"127.0.0.1:443":      errInternalAddress,
		"10.1.2.3:443":       errInternalAddress,
	} {
		if got := dialAllowed(addr); !errors.Is(got, want) || (want == nil) != (got == nil) {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

// TestSendOutcomes: headers on the way out, outcomes on the way back.
func TestSendOutcomes(t *testing.T) {
	var got http.Header

	status := http.StatusCreated

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(status)
	}))
	defer srv.Close()

	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true

	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	keys := &Keys{P256dh: b64(ua.PublicKey().Bytes()), Auth: b64(make([]byte, 16))}
	s := &Sender{Client: client, VAPID: testVAPID(t), TTL: time.Hour}

	res, err := s.Send(context.Background(), srv.URL, keys, []byte(`{"@type":"StateChange"}`))
	if err != nil || res.Outcome != Delivered {
		t.Fatalf("send: %v %+v", err, res)
	}

	if got.Get("Content-Encoding") != "aes128gcm" || got.Get("TTL") != "3600" || !strings.HasPrefix(got.Get("Authorization"), "vapid t=") {
		t.Errorf("request headers %v", got)
	}

	for code, want := range map[int]Outcome{http.StatusGone: Gone, http.StatusNotFound: Gone,
		http.StatusTooManyRequests: Slow, http.StatusBadGateway: Slow, http.StatusBadRequest: Refused} {
		status = code

		res, _ := s.Send(context.Background(), srv.URL, keys, []byte(`{}`))
		if res.Outcome != want {
			t.Errorf("%d gave %v, want %v", code, res.Outcome, want)
		}

		if code == http.StatusTooManyRequests && res.RetryAfter != 30*time.Second {
			t.Errorf("Retry-After read as %v", res.RetryAfter)
		}
	}
}
