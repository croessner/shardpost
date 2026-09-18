// Package rookcreds fetches cephx credentials that Rook manages, straight
// from the Kubernetes API. Rook writes the key of a CephClient CR into a
// Secret in its own namespace (rook-ceph); a pod in another namespace can
// read it if a Role in rook-ceph grants "get" on exactly that Secret. This
// avoids copying secrets around by hand and works with key rotation.
package rookcreds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Creds is what librados needs to talk to the cluster.
type Creds struct {
	MonHost string
	Key     string
}

// Fetch reads the CephClient secret and the mon endpoint ConfigMap.
func Fetch(ctx context.Context, rookNamespace, secretName, clientName string) (*Creds, error) {
	c, err := newClient()
	if err != nil {
		return nil, err
	}

	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", rookNamespace, secretName), &secret); err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", rookNamespace, secretName, err)
	}

	enc, ok := secret.Data[clientName]
	if !ok {
		// Rook names the key after the client; fall back to a single-entry secret.
		if len(secret.Data) != 1 {
			return nil, fmt.Errorf("secret %s/%s has no key %q", rookNamespace, secretName, clientName)
		}

		for _, v := range secret.Data {
			enc = v
		}
	}

	key, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("decode cephx key: %w", err)
	}

	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := c.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/configmaps/rook-ceph-mon-endpoints", rookNamespace), &cm); err != nil {
		return nil, fmt.Errorf("read mon endpoints: %w", err)
	}

	monHost, err := monHostFromRook(cm.Data["data"])
	if err != nil {
		return nil, err
	}

	return &Creds{MonHost: monHost, Key: strings.TrimSpace(string(key))}, nil
}

// monHostFromRook turns Rook's "a=10.0.0.1:6789,b=10.0.0.2:6789" into a
// librados mon_host string using msgr2 (port 3300). The cluster only
// accepts aes256k keys, which require msgr2.
func monHostFromRook(data string) (string, error) {
	var hosts []string

	for _, ent := range strings.Split(data, ",") {
		_, addr, ok := strings.Cut(ent, "=")
		if !ok {
			continue
		}

		host := addr
		if i := strings.LastIndex(addr, ":"); i > 0 {
			host = addr[:i]
		}

		hosts = append(hosts, fmt.Sprintf("[v2:%s:3300,v1:%s:6789]", host, host))
	}

	if len(hosts) == 0 {
		return "", fmt.Errorf("no mon endpoints in %q", data)
	}

	return strings.Join(hosts, ","), nil
}

type client struct {
	http  *http.Client
	base  string
	token string
}

func newClient() (*client, error) {
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("service account token: %w", err)
	}

	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("service account CA: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)

	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST not set")
	}

	return &client{
		http:  &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
		base:  "https://" + host + ":" + port,
		token: strings.TrimSpace(string(token)),
	}, nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// Watch polls the CephClient secret and calls onChange whenever the key
// differs from the last one the caller confirmed. Rook rotates keys in
// place (same secret name), so polling is enough and needs no watch
// permission. A failed onChange is retried at the next poll; startKey is
// the key the process started with, so a rotation between startup and the
// first poll is not missed. Runs until ctx is done.
func Watch(ctx context.Context, rookNamespace, secretName, clientName, startKey string, interval time.Duration, onChange func(key string) error) {
	active := startKey

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		creds, err := Fetch(ctx, rookNamespace, secretName, clientName)
		if err != nil || creds.Key == active {
			continue
		}

		if err := onChange(creds.Key); err == nil {
			active = creds.Key
		}
	}
}
