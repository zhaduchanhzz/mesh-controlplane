package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"

	"tailscale.com/net/dnscache"
)

// The cluster listener answers only the hostname of its server_url (P08,
// S11c), and the lab has no DNS for benchmark hostnames. -resolve maps
// hostnames to addresses inside this process, without touching the shared
// load host's /etc/hosts: Go's resolver is pointed at a tiny in-process DNS
// server (one query per net.Pipe, TCP framing) that answers A records for
// the mapped names and NXDOMAIN for anything else. Tailscale's dnscache
// copies dnscache.Get().Forward when a client is created, so this must run
// before the first controlclient.NewDirect.

var errResolveFlag = errors.New("-resolve wants host=ipv4[,host=ipv4…]")

func parseResolve(s string) (map[string]netip.Addr, error) {
	m := map[string]netip.Addr{}

	for item := range strings.SplitSeq(s, ",") {
		host, ip, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || host == "" {
			return nil, fmt.Errorf("%w: %q", errResolveFlag, item)
		}

		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("%w: %q", errResolveFlag, item)
		}

		m[dnsName(host)] = addr
	}

	return m, nil
}

func dnsName(h string) string { return strings.ToLower(strings.TrimSuffix(h, ".")) + "." }

func installResolver(m map[string]netip.Addr) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go serveDNS(server, m)

			return client, nil
		},
	}

	dnscache.Get().Forward = r
	net.DefaultResolver = r
}

const (
	dnsTypeA    = 1
	dnsClassIN  = 1
	dnsHeader   = 12
	dnsNXDomain = 3
)

// serveDNS answers one query in TCP framing (2-byte length prefix), as
// Go's resolver sends it on a stream connection.
func serveDNS(c net.Conn, m map[string]netip.Addr) {
	defer c.Close()

	var l [2]byte

	_, err := io.ReadFull(c, l[:])
	if err != nil {
		return
	}

	q := make([]byte, binary.BigEndian.Uint16(l[:]))

	_, err = io.ReadFull(c, q)
	if err != nil || len(q) < dnsHeader {
		return
	}

	resp := dnsAnswer(q, m)
	if resp == nil {
		return
	}

	out := binary.BigEndian.AppendUint16(nil, uint16(len(resp))) //nolint:gosec // a DNS message is < 64 KiB
	_, _ = c.Write(append(out, resp...))
}

// dnsAnswer builds the response to query q: an A record for a mapped name,
// no answer (NOERROR) for other types of a mapped name, NXDOMAIN otherwise.
func dnsAnswer(q []byte, m map[string]netip.Addr) []byte {
	// Question: labels, then type and class.
	var labels []string

	i := dnsHeader
	for {
		if i >= len(q) {
			return nil
		}

		n := int(q[i])
		i++

		if n == 0 {
			break
		}

		if n&0xc0 != 0 || i+n > len(q) {
			return nil
		}

		labels = append(labels, string(q[i:i+n]))
		i += n
	}

	if i+4 > len(q) {
		return nil
	}

	qtype := binary.BigEndian.Uint16(q[i:])
	qend := i + 4
	name := dnsName(strings.Join(labels, "."))
	addr, known := m[name]

	flags := uint16(0x8000 | 0x0400 | 0x0080)        // QR, AA, RA
	flags |= binary.BigEndian.Uint16(q[2:]) & 0x0100 // RD as asked

	if !known {
		flags |= dnsNXDomain
	}

	answer := known && qtype == dnsTypeA

	resp := make([]byte, 0, qend+16)
	resp = append(resp, q[0], q[1])
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1) // QDCOUNT

	if answer {
		resp = binary.BigEndian.AppendUint16(resp, 1)
	} else {
		resp = binary.BigEndian.AppendUint16(resp, 0)
	}

	resp = append(resp, 0, 0, 0, 0) // NSCOUNT, ARCOUNT
	resp = append(resp, q[dnsHeader:qend]...)

	if answer {
		resp = append(resp, 0xc0, dnsHeader) // pointer to the question name
		resp = binary.BigEndian.AppendUint16(resp, dnsTypeA)
		resp = binary.BigEndian.AppendUint16(resp, dnsClassIN)
		resp = binary.BigEndian.AppendUint32(resp, 60)
		resp = binary.BigEndian.AppendUint16(resp, 4)
		a4 := addr.As4()
		resp = append(resp, a4[:]...)
	}

	return resp
}
