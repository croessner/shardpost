package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ParsePlain decodes a SASL PLAIN response ("authzid\x00authcid\x00passwd").
func ParsePlain(b64 string) (user, password string, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", fmt.Errorf("invalid base64")
	}

	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("invalid PLAIN response")
	}

	if parts[0] != "" && parts[0] != parts[1] {
		return "", "", fmt.Errorf("proxy authorization not supported")
	}

	return parts[1], parts[2], nil
}

// xoauth2UserKey is the key of the user name in an XOAUTH2 response.
const xoauth2UserKey = "user"

// ParseXOAUTH2 decodes the Google/Microsoft XOAUTH2 initial response:
// "user=<user>\x01auth=Bearer <token>\x01\x01".
func ParseXOAUTH2(b64 string) (user, token string, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", fmt.Errorf("invalid base64")
	}

	for _, kv := range strings.Split(string(raw), "\x01") {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case xoauth2UserKey:
			user = v
		case "auth":
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				token = strings.TrimSpace(v[7:])
			}
		}
	}

	if user == "" || token == "" {
		return "", "", fmt.Errorf("invalid XOAUTH2 response")
	}

	return user, token, nil
}

// ParseOAuthBearer decodes an RFC 7628 OAUTHBEARER initial response:
// "n,a=<user>,\x01host=..\x01port=..\x01auth=Bearer <token>\x01\x01".
// The authorization identity is optional; the token is mandatory.
func ParseOAuthBearer(b64 string) (user, token string, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", fmt.Errorf("invalid base64")
	}

	s := string(raw)

	gs2, rest, ok := strings.Cut(s, "\x01")
	if !ok {
		return "", "", fmt.Errorf("invalid OAUTHBEARER response")
	}

	for _, part := range strings.Split(gs2, ",") {
		if strings.HasPrefix(part, "a=") {
			user = part[2:]
		}
	}

	for _, kv := range strings.Split(rest, "\x01") {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "auth") && strings.HasPrefix(strings.ToLower(v), "bearer ") {
			token = strings.TrimSpace(v[7:])
		}
	}

	if token == "" {
		return "", "", fmt.Errorf("invalid OAUTHBEARER response")
	}

	return user, token, nil
}

// OAuthBearerFailure is the base64 JSON the server sends in the
// continuation after a failed OAUTHBEARER/XOAUTH2 attempt (RFC 7628 3.2.3,
// XOAUTH2 uses the same shape).
func OAuthBearerFailure(status int, scope string) string {
	b, _ := json.Marshal(map[string]string{"status": fmt.Sprint(status), "schemes": "bearer", "scope": scope})
	return base64.StdEncoding.EncodeToString(b)
}
