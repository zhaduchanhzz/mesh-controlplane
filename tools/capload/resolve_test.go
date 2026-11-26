package main

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// TestInProcessResolver: -resolve answers mapped names (any case, trailing
// dot) with their IPv4 and nothing else, through Go's own resolver.
func TestInProcessResolver(t *testing.T) {
	m, err := parseResolve("Bench.Lab.Internal=192.0.2.10, other.example=192.0.2.11")
	if err != nil {
		t.Fatal(err)
	}

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveDNS(server, m)

			return client, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, host := range []string{"bench.lab.internal", "BENCH.lab.internal.", "other.example"} {
		ips, err := r.LookupNetIP(ctx, "ip", host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}

		want := m[dnsName(host)]
		if len(ips) != 1 || ips[0].Unmap() != want {
			t.Fatalf("%s: got %v, want %v", host, ips, want)
		}
	}

	_, err = r.LookupNetIP(ctx, "ip", "unmapped.lab.internal")
	if err == nil {
		t.Fatal("an unmapped name resolved")
	}

	for _, bad := range []string{"", "host", "host=", "host=::1", "=192.0.2.1", "host=not-an-ip"} {
		if _, err := parseResolve(bad); err == nil { //nolint:noinlineerr
			t.Errorf("parseResolve(%q) accepted", bad)
		}
	}

	if markerEndpoint(1) == markerEndpoint(2) || !netip.MustParsePrefix("198.18.0.0/15").Contains(markerEndpoint(7).Addr()) {
		t.Fatal("marker endpoints must be unique and in 198.18.0.0/15")
	}
}
