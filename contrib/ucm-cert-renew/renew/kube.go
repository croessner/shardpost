package renew

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lastAppliedAnnotation is where kubectl apply keeps the applied object.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// serviceAccountDir holds the pod's API credentials.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// SecretStore reads and replaces one Secret through the Kubernetes API
// with the pod's service account (needs get and update on that Secret
// only; the Secret must exist).
type SecretStore struct {
	Server    string // https://host:port
	Token     func() (string, error)
	Client    *http.Client
	Namespace string
	Name      string
}

// NewInClusterStore builds a SecretStore from the service account mount;
// namespace "" is the pod's own.
func NewInClusterStore(namespace, name string) (*SecretStore, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod")
	}

	caPEM, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("service account CA is empty")
	}

	if namespace == "" {
		ns, err := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
		if err != nil {
			return nil, err
		}

		namespace = strings.TrimSpace(string(ns))
	}

	return &SecretStore{
		Server: "https://" + net.JoinHostPort(host, port),
		Token: func() (string, error) {
			t, err := os.ReadFile(filepath.Join(serviceAccountDir, "token"))
			return strings.TrimSpace(string(t)), err
		},
		Client: &http.Client{Timeout: 20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}},
		Namespace: namespace, Name: name,
	}, nil
}

func (s *SecretStore) url() string {
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", s.Server, url.PathEscape(s.Namespace), url.PathEscape(s.Name))
}

// Load implements Store. The token handed to Save is the object as read;
// its resourceVersion makes the later replace conditional.
func (s *SecretStore) Load(ctx context.Context) (map[string][]byte, string, error) {
	body, err := s.do(ctx, http.MethodGet, nil)
	if err != nil {
		return nil, "", err
	}

	var obj struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, "", err
	}

	data := map[string][]byte{}

	for k, v := range obj.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, "", fmt.Errorf("secret key %s: %w", k, err)
		}

		data[k] = b
	}

	return data, string(body), nil
}

// Save implements Store: it replaces the data of the object read by Load;
// its resourceVersion makes a concurrent change fail instead of being
// overwritten.
func (s *SecretStore) Save(ctx context.Context, data map[string][]byte, token string) error {
	var obj map[string]any
	if err := json.Unmarshal([]byte(token), &obj); err != nil {
		return fmt.Errorf("secret object: %w", err)
	}

	enc := map[string]string{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString(v)
	}

	obj["data"] = enc
	delete(obj, "stringData")
	// A former "kubectl apply" leaves the old data, keys included, in
	// this annotation; the Secret is managed here from now on.
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if ann, ok := meta["annotations"].(map[string]any); ok {
			delete(ann, lastAppliedAnnotation)
		}
	}

	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	_, err = s.do(ctx, http.MethodPut, body)

	return err
}

// do performs one API request and returns the body of a 2xx answer.
func (s *SecretStore) do(ctx context.Context, method string, body []byte) ([]byte, error) {
	token, err := s.Token()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, s.url(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("kubernetes API %s: HTTP %d", method, resp.StatusCode)
	}

	return out, nil
}
