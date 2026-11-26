package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// header builds a v2 header: cmd 0 LOCAL / 1 PROXY, famProto 0x11 TCP4,
// 0x21 TCP6, 0x00 UNSPEC; tlv bytes are appended to the address block.
func header(cmd, famProto byte, src netip.AddrPort, tlv []byte) []byte {
	var block []byte

	dst := netip.MustParseAddrPort("192.0.2.200:8444")

	switch famProto {
	case 0x11:
		s, d := src.Addr().As4(), dst.Addr().As4()
		block = append(block, s[:]...)
		block = append(block, d[:]...)
		block = binary.BigEndian.AppendUint16(block, src.Port())
		block = binary.BigEndian.AppendUint16(block, dst.Port())
	case 0x21:
		s, d := src.Addr().As16(), netip.MustParseAddr("2001:db8::200").As16()
		block = append(block, s[:]...)
		block = append(block, d[:]...)
		block = binary.BigEndian.AppendUint16(block, src.Port())
		block = binary.BigEndian.AppendUint16(block, 8444)
	}

	block = append(block, tlv...)

	h := append([]byte{}, sigV2...)
	h = append(h, 0x20|cmd, famProto)
	h = binary.BigEndian.AppendUint16(h, uint16(len(block)))

	return append(h, block...)
}

type result struct {
	remote  string
	payload string
	err     error
}

// serve accepts one connection on a Listener over loopback with trusted,
// sends what the client writes, and reports what the server saw.
func serve(t *testing.T, trusted []netip.Prefix, timeout time.Duration, send func(net.Conn)) (result, []string) {
	t.Helper()

	inner, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		refused []string
	)

	l, err := NewListener(inner, Config{Trusted: trusted, HeaderTimeout: timeout, Refused: func(r string) {
		mu.Lock()
		refused = append(refused, r)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })

	out := make(chan result, 1)

	go func() {
		c, err := l.Accept()
		if err != nil {
			out <- result{err: err}

			return
		}
		defer c.Close()

		remote := c.RemoteAddr().String()
		b, err := io.ReadAll(io.LimitReader(c, 64))
		out <- result{remote: remote, payload: string(b), err: err}
	}()

	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	send(conn)

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	defer conn.Close()

	select {
	case r := <-out:
		mu.Lock()
		defer mu.Unlock()

		return r, append([]string(nil), refused...)
	case <-time.After(10 * time.Second):
		t.Fatal("server never finished")

		return result{}, nil
	}
}

var (
	loopback  = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	elsewhere = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
)

func TestTrustedV4HeaderGivesClientAddress(t *testing.T) {
	r, refused := serve(t, loopback, time.Second, func(c net.Conn) {
		_, _ = c.Write(header(1, 0x11, netip.MustParseAddrPort("203.0.113.7:40000"), nil))
		_, _ = c.Write([]byte("hello"))
	})

	if r.remote != "203.0.113.7:40000" || r.payload != "hello" || len(refused) != 0 {
		t.Fatalf("remote %q payload %q err %v refused %v", r.remote, r.payload, r.err, refused)
	}
}

func TestTrustedV6HeaderWithTLVsGivesClientAddress(t *testing.T) {
	r, refused := serve(t, loopback, time.Second, func(c net.Conn) {
		// Byte by byte: the header may arrive in any number of segments.
		for _, b := range header(1, 0x21, netip.MustParseAddrPort("[2001:db8:1:2::9]:5555"), []byte{0x04, 0x00, 0x01, 0x00}) {
			_, _ = c.Write([]byte{b})
		}

		_, _ = c.Write([]byte("x"))
	})

	if r.remote != "[2001:db8:1:2::9]:5555" || r.payload != "x" || len(refused) != 0 {
		t.Fatalf("remote %q payload %q err %v refused %v", r.remote, r.payload, r.err, refused)
	}
}

func TestTrustedLocalAndUnspecKeepPeer(t *testing.T) {
	for name, h := range map[string][]byte{
		"local":  header(0, 0x00, netip.AddrPort{}, nil),
		"unspec": header(1, 0x00, netip.AddrPort{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			r, refused := serve(t, loopback, time.Second, func(c net.Conn) {
				_, _ = c.Write(h)
				_, _ = c.Write([]byte("ok"))
			})

			if ap, err := netip.ParseAddrPort(r.remote); err != nil || !ap.Addr().IsLoopback() || r.payload != "ok" || len(refused) != 0 {
				t.Fatalf("remote %q payload %q refused %v", r.remote, r.payload, refused)
			}
		})
	}
}

// The front door must send a header: a trusted peer without one would make
// every client share the front door's address.
func TestTrustedPeerWithoutValidHeaderRefused(t *testing.T) {
	src := netip.MustParseAddrPort("203.0.113.7:1")

	long := header(1, 0x11, src, nil)
	binary.BigEndian.PutUint16(long[14:16], maxBlock+1)

	badVersion := header(1, 0x11, src, nil)
	badVersion[12] = 0x11

	badCmd := header(1, 0x11, src, nil)
	badCmd[12] = 0x2F

	short := header(1, 0x11, src, nil)
	binary.BigEndian.PutUint16(short[14:16], 4)

	cases := map[string]struct {
		send []byte
		want string
	}{
		"none (TLS ClientHello)": {[]byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc, 0x03, 0x03, 0, 0, 0, 0, 0, 0}, ReasonMissingHeader},
		"v1 text":                {[]byte("PROXY TCP4 203.0.113.7 192.0.2.1 1 2\r\n"), ReasonMissingHeader},
		"too long":               {append(long, make([]byte, maxBlock+1)...), ReasonMalformed},
		"version 1 nibble":       {badVersion, ReasonMalformed},
		"unknown command":        {badCmd, ReasonMalformed},
		"short v4 block":         {short[:20], ReasonMalformed},
		"udp":                    {header(1, 0x12, src, nil), ReasonUnsupported},
		"unix":                   {append(header(1, 0x00, src, nil)[:13], 0x31, 0, 0), ReasonUnsupported},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, refused := serve(t, loopback, time.Second, func(c net.Conn) {
				_, _ = c.Write(tc.send)
				_, _ = c.Write([]byte("payload"))
			})

			if r.err == nil || r.payload != "" || len(refused) != 1 || refused[0] != tc.want {
				t.Fatalf("payload %q err %v refused %v, want refused [%s]", r.payload, r.err, refused, tc.want)
			}
		})
	}
}

func TestTrustedPeerSilentTimesOut(t *testing.T) {
	start := time.Now()

	r, refused := serve(t, loopback, 200*time.Millisecond, func(c net.Conn) {
		time.Sleep(600 * time.Millisecond)
	})

	if r.err == nil || len(refused) != 1 || refused[0] != ReasonTimeout {
		t.Fatalf("err %v refused %v, want a timeout", r.err, refused)
	}

	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("refused after %s", d)
	}
}

// Spoofing: a peer outside the trusted front doors cannot choose the
// address it is limited as. A PROXY header from it closes the connection;
// without one it is served with its own address.
func TestUntrustedPeerCannotSendHeader(t *testing.T) {
	for name, h := range map[string][]byte{
		"v2": header(1, 0x11, netip.MustParseAddrPort("198.51.100.9:1"), nil),
		"v1": []byte("PROXY TCP4 198.51.100.9 192.0.2.1 1 2\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			r, refused := serve(t, elsewhere, time.Second, func(c net.Conn) {
				_, _ = c.Write(h)
				_, _ = c.Write([]byte("payload"))
			})

			if r.err == nil || r.payload != "" || len(refused) != 1 || refused[0] != ReasonUntrustedHeader {
				t.Fatalf("remote %q payload %q err %v refused %v, want refused [%s]",
					r.remote, r.payload, r.err, refused, ReasonUntrustedHeader)
			}

			if ap, err := netip.ParseAddrPort(r.remote); err != nil || !ap.Addr().IsLoopback() {
				t.Fatalf("remote %q: the spoofed address leaked", r.remote)
			}
		})
	}

	for name, payload := range map[string][]byte{
		"tls":  {0x16, 0x03, 0x01, 0x00, 0x05, 1, 2, 3, 4, 5},
		"http": []byte("POST /v1/bootstrap HTTP/1.1\r\n"),
		"one":  []byte("P"),
	} {
		t.Run("plain "+name, func(t *testing.T) {
			r, refused := serve(t, elsewhere, 300*time.Millisecond, func(c net.Conn) {
				_, _ = c.Write(payload)
			})

			if name == "one" {
				// "P" then EOF: the start of a v1 signature; refused either way.
				return
			}

			if r.err != nil || !bytes.Equal([]byte(r.payload), payload) || len(refused) != 0 {
				t.Fatalf("payload %q err %v refused %v", r.payload, r.err, refused)
			}

			if ap, err := netip.ParseAddrPort(r.remote); err != nil || !ap.Addr().IsLoopback() {
				t.Fatalf("remote %q, want the TCP peer", r.remote)
			}
		})
	}
}

func TestNewListenerValidation(t *testing.T) {
	inner, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer inner.Close()

	for name, tr := range map[string][]netip.Prefix{
		"empty": nil,
		"v4 /0": {netip.MustParsePrefix("0.0.0.0/0")},
		"v6 /0": {netip.MustParsePrefix("::/0")},
	} {
		if _, err := NewListener(inner, Config{Trusted: tr}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A deadline the server set before the header was read survives it.
func TestCallerReadDeadlineKept(t *testing.T) {
	inner, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	l, err := NewListener(inner, Config{Trusted: loopback, HeaderTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	done := make(chan error, 1)

	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- err

			return
		}
		defer c.Close()

		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_ = c.RemoteAddr() // reads the header

		_, err = c.Read(make([]byte, 1)) // nothing more comes: the caller's deadline fires
		done <- err
	}()

	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Close()

	_, _ = conn.Write(header(1, 0x11, netip.MustParseAddrPort("203.0.113.7:1"), nil))

	select {
	case err := <-done:
		var ne net.Error
		if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("read after the header: err %v, want the caller's deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the caller's read deadline was replaced by none")
	}
}
