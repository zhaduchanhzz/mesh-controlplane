// Package proxyproto accepts the PROXY protocol version 2 header (HAProxy,
// "The PROXY protocol", §2.2) on a TCP listener, so a service behind a TCP
// front door sees the real client address (P13f, B7: per-address limits of
// the public bootstrap endpoint).
//
// The header is trusted only from the front door:
//
//   - a peer inside Trusted must start every connection with a v2 header;
//     a connection without one (or with a v1 text header, a malformed one,
//     an unsupported family, or none within HeaderTimeout) is closed: a
//     front door that stopped sending headers would otherwise make every
//     client look like the front door and share its limits;
//   - a peer outside Trusted is served with its own address, and a
//     connection from it that starts with a PROXY header (v1 or v2) is
//     closed: a client must not choose the address it is limited as.
//
// LOCAL (the front door's own health check) and UNSPEC headers keep the
// peer address. Only TCP over IPv4 and IPv6 is accepted. The header is read
// on the connection's goroutine (first Read or RemoteAddr), never in
// Accept, so a silent peer cannot stall the accept loop.
//
// It is written here rather than taken from github.com/pires/go-proxyproto
// (only an indirect dependency) so the module graph and the nix vendor hash
// stay unchanged; it implements only the subset above.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Reasons a connection is refused (Config.Refused).
const (
	ReasonMissingHeader   = "missing_header"   // trusted peer, no v2 header
	ReasonMalformed       = "malformed"        // trusted peer, bad version, command or length
	ReasonUnsupported     = "unsupported"      // trusted peer, a family other than TCP over IPv4/IPv6
	ReasonTimeout         = "timeout"          // no header (or no first bytes) within HeaderTimeout
	ReasonUntrustedHeader = "untrusted_header" // a PROXY header from a peer outside Trusted
)

// DefaultHeaderTimeout bounds the wait for the header.
const DefaultHeaderTimeout = 5 * time.Second

// maxBlock bounds the address block plus TLVs accepted after the 16-byte
// fixed part (the spec allows 65535; HAProxy sends 12 or 36 bytes plus any
// TLVs it is configured to add).
const maxBlock = 512

var (
	// sigV2 is the v2 signature.
	sigV2 = []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}
	// sigV1 is the start of a v1 (text) header.
	sigV1 = []byte("PROXY ")

	errNoTrusted   = errors.New("proxyproto: at least one trusted front door prefix is required")
	errZeroPrefix  = errors.New("proxyproto: a /0 prefix would trust every peer")
	errRefusedConn = errors.New("proxyproto: connection refused")
)

// Config configures a [Listener].
type Config struct {
	// Trusted are the front doors allowed (and required) to send the header.
	Trusted []netip.Prefix
	// HeaderTimeout bounds the wait for the header; 0 means
	// [DefaultHeaderTimeout].
	HeaderTimeout time.Duration
	// Refused, if set, is called once per refused connection with its
	// reason (metrics, logs). It must not block.
	Refused func(reason string)
}

// Listener wraps a TCP listener; its connections report the client address
// from the PROXY v2 header of a trusted front door.
type Listener struct {
	net.Listener

	cfg Config
}

// NewListener wraps l. cfg.Trusted must be non-empty and hold no /0.
func NewListener(l net.Listener, cfg Config) (*Listener, error) {
	if len(cfg.Trusted) == 0 {
		return nil, errNoTrusted
	}

	for _, p := range cfg.Trusted {
		if !p.IsValid() || p.Bits() == 0 {
			return nil, fmt.Errorf("%w: %s", errZeroPrefix, p)
		}
	}

	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = DefaultHeaderTimeout
	}

	return &Listener{Listener: l, cfg: cfg}, nil
}

// Accept returns the next connection; its header is read on first use.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &Conn{Conn: c, cfg: &l.cfg, br: bufio.NewReader(c)}, nil
}

// Conn is a connection accepted by [Listener].
type Conn struct {
	net.Conn

	cfg *Config
	br  *bufio.Reader

	once   sync.Once
	remote net.Addr // nil: the TCP peer
	err    error

	mu           sync.Mutex
	readDeadline time.Time // the deadline the caller set, restored after the header
}

// RemoteAddr is the client address from the header, or the TCP peer.
func (c *Conn) RemoteAddr() net.Addr {
	c.init()

	if c.remote != nil {
		return c.remote
	}

	return c.Conn.RemoteAddr()
}

// Read reads after the header; a refused connection reads an error.
func (c *Conn) Read(b []byte) (int, error) {
	c.init()

	if c.err != nil {
		return 0, c.err
	}

	return c.br.Read(b)
}

// SetDeadline is net.Conn's; the read deadline is remembered so the header
// wait does not replace it.
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()

	return c.Conn.SetDeadline(t)
}

// SetReadDeadline is net.Conn's, remembered as for SetDeadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()

	return c.Conn.SetReadDeadline(t)
}

func (c *Conn) init() {
	c.once.Do(func() {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.HeaderTimeout))

		reason := c.readHeader()

		c.mu.Lock()
		_ = c.Conn.SetReadDeadline(c.readDeadline)
		c.mu.Unlock()

		if reason == "" {
			return
		}

		c.err = fmt.Errorf("%w: %s", errRefusedConn, reason)
		_ = c.Conn.Close()

		if c.cfg.Refused != nil {
			c.cfg.Refused(reason)
		}
	})
}

// readHeader consumes the header of a trusted peer, or checks that an
// untrusted one sends none. It returns the refusal reason, or "".
func (c *Conn) readHeader() string {
	if !c.trustedPeer() {
		return c.refuseHeader()
	}

	var fixed [16]byte

	_, err := io.ReadFull(c.br, fixed[:])
	if err != nil {
		if isTimeout(err) {
			return ReasonTimeout
		}

		return ReasonMissingHeader
	}

	if !bytes.Equal(fixed[:12], sigV2) {
		return ReasonMissingHeader
	}

	verCmd, famProto := fixed[12], fixed[13]
	n := int(binary.BigEndian.Uint16(fixed[14:16]))

	if verCmd>>4 != 2 || n > maxBlock {
		return ReasonMalformed
	}

	block := make([]byte, n)

	_, err = io.ReadFull(c.br, block)
	if err != nil {
		if isTimeout(err) {
			return ReasonTimeout
		}

		return ReasonMalformed
	}

	switch verCmd & 0x0F {
	case 0x0: // LOCAL: the front door's own connection (health check)
		return ""
	case 0x1: // PROXY
	default:
		return ReasonMalformed
	}

	switch famProto {
	case 0x00: // UNSPEC: keep the peer address
		return ""
	case 0x11: // TCP over IPv4: src, dst, src port, dst port
		if n < 12 {
			return ReasonMalformed
		}

		src := netip.AddrFrom4([4]byte(block[0:4]))
		c.remote = net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, binary.BigEndian.Uint16(block[8:10])))
	case 0x21: // TCP over IPv6
		if n < 36 {
			return ReasonMalformed
		}

		src := netip.AddrFrom16([16]byte(block[0:16])).Unmap()
		c.remote = net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, binary.BigEndian.Uint16(block[32:34])))
	default:
		return ReasonUnsupported
	}

	return ""
}

// refuseHeader: a peer outside Trusted may not send a PROXY header. Only a
// first byte that can start one (CR for v2, 'P' for v1) makes it look
// further; a TLS ClientHello starts with 0x16.
func (c *Conn) refuseHeader() string {
	first, err := c.br.Peek(1)
	if err != nil {
		return "" // EOF or the peer's own timeout: the server sees it on Read
	}

	var sig []byte

	switch first[0] {
	case sigV2[0]:
		sig = sigV2
	case sigV1[0]:
		sig = sigV1
	default:
		return ""
	}

	got, _ := c.br.Peek(len(sig)) // fewer bytes on EOF or timeout
	if !bytes.HasPrefix(sig, got) {
		return "" // not a PROXY header (e.g. "POST /...")
	}

	if len(got) < len(sig) {
		return ReasonTimeout // the start of a signature, then silence or EOF
	}

	return ReasonUntrustedHeader
}

func (c *Conn) trustedPeer() bool {
	ap, err := netip.ParseAddrPort(c.Conn.RemoteAddr().String())
	if err != nil {
		return false
	}

	ip := ap.Addr().Unmap()

	for _, p := range c.cfg.Trusted {
		if p.Contains(ip) {
			return true
		}
	}

	return false
}

func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// ParseTrusted parses a comma-separated CIDR list of front doors ("" is
// none: PROXY protocol off). A /0 is refused.
func ParseTrusted(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix

	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}

		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("proxyproto: front door CIDR %q: %w", f, err)
		}

		if p.Bits() == 0 {
			return nil, fmt.Errorf("%w: %s", errZeroPrefix, p)
		}

		out = append(out, p.Masked())
	}

	return out, nil
}
