package frontdoor

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/tailnet"
)

var (
	tnA = tailnet.MustParseID("0b6f4c2e-8a1d-4f3b-9c7e-2d5a1b3c4e5f")
	tnB = tailnet.MustParseID("7e9d1a3b-5c2f-4e8a-b6d4-1f0c3a5b7d9e")
)

func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"a.cp.example.com":       "a.cp.example.com",
		"A.CP.Example.COM":       "a.cp.example.com",
		"a.cp.example.com:8443":  "a.cp.example.com",
		"a.cp.example.com:80":    "a.cp.example.com",
		"a.cp.example.com.":      "a.cp.example.com",
		"a.cp.example.com.:8443": "a.cp.example.com",
	}
	for in, want := range ok {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{
		"", ":8443", "116.118.3.46", "116.118.3.46:8443", "[::1]:8443", "::1",
		"a.cp.example.com:", "a.cp.example.com:abc", "a..b", "-a.example.com",
		"a_b.example.com", "a.example.com/evil", "a.example.com\r\nx: y",
		strings.Repeat("a", 64) + ".example.com",
	} {
		if got, err := NormalizeHost(in); !errors.Is(err, ErrBadHost) {
			t.Errorf("NormalizeHost(%q) = %q, %v; want ErrBadHost", in, got, err)
		}
	}
}

func TestNewRejectsDuplicateAndInvalidBackends(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	if _, err := New([]Backend{
		{Tailnet: tnA, Hosts: []string{"a.example.com"}, Target: u},
		{Tailnet: tnB, Hosts: []string{"A.example.com:8443"}, Target: u},
	}); err == nil {
		t.Fatal("duplicate host across tailnets accepted")
	}

	if _, err := New([]Backend{{Hosts: []string{"a.example.com"}, Target: u}}); err == nil {
		t.Fatal("backend without tailnet accepted")
	}

	if _, err := New([]Backend{{Tailnet: tnA, Hosts: nil, Target: u}}); err == nil {
		t.Fatal("backend without hosts accepted")
	}

	if _, err := New([]Backend{{Tailnet: tnA, Hosts: []string{"10.0.0.1"}, Target: u}}); err == nil {
		t.Fatal("IP host accepted")
	}

	if _, err := New(nil); err == nil {
		t.Fatal("empty door accepted")
	}
}

// echoBackend answers with its own name and the tailnet header it saw.
func echoBackend(t *testing.T, name string) (*httptest.Server, *url.URL) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", name)
		w.Header().Set("X-Seen-Tailnet-Header", r.Header.Get("X-Tailnet-ID"))
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)

	return srv, u
}

func newDoor(t *testing.T) *Door {
	t.Helper()
	_, ua := echoBackend(t, "A")
	_, ub := echoBackend(t, "B")

	d, err := New([]Backend{
		{Tailnet: tnA, Hosts: []string{"a.cp.example.com"}, Target: ua},
		{Tailnet: tnB, Hosts: []string{"b.cp.example.com"}, Target: ub},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func do(t *testing.T, d http.Handler, host, sni string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/key?v=116", nil)

	req.Host = host
	if sni != "-" {
		req.TLS = &tls.ConnectionState{ServerName: sni}
	} else {
		req.TLS = nil
	}

	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)

	return rec
}

func TestRoutesByHost(t *testing.T) {
	d := newDoor(t)
	for host, want := range map[string]string{
		"a.cp.example.com":      "A",
		"a.cp.example.com:8443": "A",
		"A.CP.EXAMPLE.COM:80":   "A",
		"b.cp.example.com:8443": "B",
	} {
		rec := do(t, d, host, "-", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("X-Backend") != want {
			t.Errorf("Host %q -> %d %q, want 200 from %s", host, rec.Code, rec.Header().Get("X-Backend"), want)
		}
	}
}

func TestUnknownHostFailsClosed(t *testing.T) {
	d := newDoor(t)
	for _, host := range []string{"zz.cp.example.com", "116.118.3.46:8443", "", "cp.example.com"} {
		rec := do(t, d, host, "-", nil)
		if rec.Code != http.StatusMisdirectedRequest || rec.Header().Get("X-Backend") != "" {
			t.Errorf("Host %q -> %d backend %q, want 421 and no backend", host, rec.Code, rec.Header().Get("X-Backend"))
		}
	}
}

func TestSNIMustMatchHost(t *testing.T) {
	d := newDoor(t)
	if rec := do(t, d, "a.cp.example.com:8443", "a.cp.example.com", nil); rec.Code != http.StatusOK {
		t.Fatalf("matching SNI -> %d", rec.Code)
	}

	for _, sni := range []string{"b.cp.example.com", "", "zz.cp.example.com"} {
		rec := do(t, d, "a.cp.example.com:8443", sni, nil)
		if rec.Code != http.StatusMisdirectedRequest || rec.Header().Get("X-Backend") != "" {
			t.Errorf("SNI %q Host A -> %d backend %q, want 421", sni, rec.Code, rec.Header().Get("X-Backend"))
		}
	}
}

func TestClientTailnetHeaderIgnored(t *testing.T) {
	d := newDoor(t)

	rec := do(t, d, "a.cp.example.com", "-", map[string]string{"X-Tailnet-ID": tnB.String()})
	if rec.Header().Get("X-Backend") != "A" {
		t.Fatalf("X-Tailnet-ID changed routing to %q", rec.Header().Get("X-Backend"))
	}

	if seen := rec.Header().Get("X-Seen-Tailnet-Header"); seen != "" {
		t.Fatalf("client X-Tailnet-ID reached backend: %q", seen)
	}
}

// TestUpgradePassesThrough checks the Tailscale-style HTTP upgrade (101 and
// then raw bytes both ways) survives the proxy, and is refused before any
// upgrade for an unknown host.
func TestUpgradePassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "tailscale-control-protocol" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}

		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: tailscale-control-protocol\r\nConnection: upgrade\r\n\r\n")
		line, _ := brw.ReadString('\n')
		_, _ = io.WriteString(conn, "echo:"+line)
	}))
	defer backend.Close()

	bu, _ := url.Parse(backend.URL)

	d, err := New([]Backend{{Tailnet: tnA, Hosts: []string{"a.cp.example.com"}, Target: bu}})
	if err != nil {
		t.Fatal(err)
	}

	front := httptest.NewServer(d)
	defer front.Close()

	upgrade := func(host string) (string, string) {
		c, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()

		_, _ = io.WriteString(c, "POST /ts2021 HTTP/1.1\r\nHost: "+host+"\r\nUpgrade: tailscale-control-protocol\r\nConnection: upgrade\r\nContent-Length: 0\r\n\r\n")
		br := bufio.NewReader(c)

		status, _ := br.ReadString('\n')
		if !strings.Contains(status, " 101 ") {
			return status, ""
		}

		for {
			l, err := br.ReadString('\n')
			if err != nil || l == "\r\n" {
				break
			}
		}

		_, _ = io.WriteString(c, "hello\n")
		reply, _ := br.ReadString('\n')

		return status, reply
	}

	if status, reply := upgrade("a.cp.example.com:8443"); !strings.Contains(status, " 101 ") || reply != "echo:hello\n" {
		t.Fatalf("known host upgrade: status %q reply %q", status, reply)
	}

	if status, _ := upgrade("zz.cp.example.com:8443"); !strings.Contains(status, " 421 ") {
		t.Fatalf("unknown host upgrade: status %q, want 421 before upgrade", status)
	}
}
