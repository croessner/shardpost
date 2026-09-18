package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
)

func TestLevelFiltering(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			var out bytes.Buffer

			log := New(&out, level)
			log.Debug("debug")
			log.Info("info")
			log.Warn("warn")
			log.Error("error")

			expected := map[string]int{"debug": 4, "info": 3, "warn": 2, "error": 1}[level]
			if strings.Count(out.String(), "\n") != expected {
				t.Fatal("wrong level filtering")
			}
		})
	}
}

func TestProxyClientAddressInSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = ln.Close() }()

	policy, err := proxyproto.PolicyFromRanges([]string{"127.0.0.1/32"}, proxyproto.USE, proxyproto.IGNORE)
	if err != nil {
		t.Fatal(err)
	}

	proxy := &proxyproto.Listener{Listener: ln, ConnPolicy: policy, ReadHeaderTimeout: time.Second}

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = client.Close() }()

	if _, err := fmt.Fprint(client, "PROXY TCP4 203.0.113.42 192.0.2.1 54321 1143\r\n"); err != nil {
		t.Fatal(err)
	}

	conn, err := proxy.Accept()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = conn.Close() }()

	var out bytes.Buffer

	log := Session(slog.New(slog.NewJSONHandler(&out, nil)), conn)
	end := Begin(log, false)
	end()

	var record map[string]any
	if err := json.Unmarshal(bytes.Split(out.Bytes(), []byte("\n"))[0], &record); err != nil {
		t.Fatal(err)
	}

	if record["remote"] != "203.0.113.42:54321" {
		t.Fatalf("effective client lost: %v", record["remote"])
	}

	if record["session_id"] == "" {
		t.Fatal("missing session id")
	}
}
