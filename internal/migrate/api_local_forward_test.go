package migrate

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestMigrationLocalForwardPreservesTLSHostname(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" {
			t.Errorf("incorrect host %s", r.Host)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://example.com")
	transport, err := migrationClientTransport(target, port)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.TLSClientConfig.RootCAs = roots
	client := &http.Client{Transport: transport}
	resp, err := client.Get(target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
func TestMigrationLocalForwardRejectsUnsafePorts(t *testing.T) {
	target, _ := url.Parse("https://example.com")
	for _, p := range []string{"443", "0", "65536", "01", "-1", "127.0.0.1:1234"} {
		if _, err := migrationClientTransport(target, p); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	target, _ = url.Parse("http://example.com")
	if _, err := migrationClientTransport(target, "18443"); err == nil {
		t.Fatal("accepted http")
	}
	target, _ = url.Parse("https://example.com:8443")
	if _, err := migrationClientTransport(target, "18443"); err == nil {
		t.Fatal("accepted nonstandard remote port")
	}
}

func TestMigrationLocalForwardDoesNotRedirectSource(t *testing.T) {
	source, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, _ := url.Parse("https://example.com")
	transport, err := migrationClientTransport(target, "18443")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := transport.DialContext(context.Background(), "tcp", source.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
