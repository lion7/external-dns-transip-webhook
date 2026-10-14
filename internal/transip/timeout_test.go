package transip

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/transip/gotransip/v6"
)

// An API that does not answer is an error well within the time ExternalDNS
// waits for the webhook, not a call that hangs until ExternalDNS gives up.
func TestRecordsGivesUpOnAnAPIThatDoesNotAnswer(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer api.Close()
	defer close(release)

	p, err := newWithConfiguration(gotransip.ClientConfiguration{
		AccountName:    "example",
		PrivateKeyPath: privateKeyFile(t),
		URL:            api.URL,
		HTTPClient:     &http.Client{Timeout: 100 * time.Millisecond},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = p.Records(context.Background())
	if err == nil {
		t.Fatal("Records succeeded against an API that does not answer")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Records gave up after %s", elapsed)
	}
}

func TestNewRefusesNoTimeout(t *testing.T) {
	if _, err := New("example", privateKeyFile(t), 0, nil, false); err == nil {
		t.Fatal("New accepted a timeout of 0, which never gives up")
	}
}

func privateKeyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "privatekey")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
