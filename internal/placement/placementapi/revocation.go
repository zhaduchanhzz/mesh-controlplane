package placementapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Operator certificate revocation (P13f, review L5). The operator CA is
// pinned and private, so instead of CRL/OCSP (N5) the operator keeps a
// revocation file on the placement host:
//
//	# one entry per line; '#' starts a comment
//	serial:0a1b2c...          # certificate serial number (hex, ':' allowed)
//	sha256:9f86d081884c7d...  # SHA-256 of the certificate (DER), any cert of the chain
//
// The file is read at startup (a bad file stops the service) and again
// every reload interval. A file that cannot be read or parsed later fails
// closed: every request is refused with 503 until a good file is in place,
// so a typo cannot silently un-revoke a certificate.

var (
	// ErrRevoked reports a request whose certificate chain holds a revoked
	// certificate.
	ErrRevoked = errors.New("operator certificate is revoked")
	// ErrRevocationUnavailable reports that the revocation file could not be
	// read or parsed on the last reload: nobody is admitted (fail closed).
	ErrRevocationUnavailable = errors.New("operator certificate revocation list unavailable")

	errRevocationLine = errors.New("invalid revocation entry")
)

var (
	revocationEntries = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "mesh_placement",
		Name:      "revoked_certificates",
		Help:      "Entries in the operator certificate revocation file last loaded.",
	})
	revocationOK = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "mesh_placement",
		Name:      "revocation_list_ok",
		Help:      "1 when the last read of the revocation file succeeded, 0 while every operator request is refused (fail closed).",
	})
	revocationReloadErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "mesh_placement",
		Name:      "revocation_reload_errors_total",
		Help:      "Failed reads or parses of the operator certificate revocation file.",
	})
)

// Revocations is the operator certificate revocation list of one file.
type Revocations struct {
	path string

	mu      sync.RWMutex
	serials map[string]bool // big.Int hex, lowercase, no leading zeros
	digests map[string]bool // SHA-256 of the DER certificate, lowercase hex
	err     error           // last load failed: refuse everything
	sum     [32]byte        // of the file last loaded
}

// LoadRevocations reads path; an unreadable or invalid file is an error.
func LoadRevocations(path string) (*Revocations, error) {
	r := &Revocations{path: path}

	err := r.Reload()
	if err != nil {
		return nil, err
	}

	return r, nil
}

// Reload reads the file again. On failure the list fails closed until a
// later Reload succeeds.
func (r *Revocations) Reload() error {
	raw, err := os.ReadFile(r.path)
	if err == nil {
		var serials, digests map[string]bool

		serials, digests, err = parseRevocations(raw)
		if err == nil {
			sum := sha256.Sum256(raw)

			r.mu.Lock()
			r.serials, r.digests, r.err, r.sum = serials, digests, nil, sum
			r.mu.Unlock()

			revocationEntries.Set(float64(len(serials) + len(digests)))
			revocationOK.Set(1)

			return nil
		}
	}

	err = fmt.Errorf("%w: %s: %w", ErrRevocationUnavailable, r.path, err)

	r.mu.Lock()
	r.err = err
	r.mu.Unlock()

	revocationReloadErrors.Inc()
	revocationOK.Set(0)

	return err
}

// Run reloads every interval until ctx ends; logf reports failures and
// changes.
func (r *Revocations) Run(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.RLock()
			before, failed := r.sum, r.err != nil
			r.mu.RUnlock()

			err := r.Reload()

			r.mu.RLock()
			after := r.sum
			r.mu.RUnlock()

			switch {
			case err != nil:
				logf("revocation list: %v (refusing every operator request until fixed)", err)
			case failed || before != after:
				logf("revocation list reloaded from %s", r.path)
			}
		}
	}
}

// Check refuses a chain holding a revoked certificate, and everything while
// the list is unavailable.
func (r *Revocations) Check(chain []*x509.Certificate) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.err != nil {
		return r.err
	}

	for i, c := range chain {
		d := sha256.Sum256(c.Raw)
		if r.digests[hex.EncodeToString(d[:])] {
			return ErrRevoked
		}

		// Serials are unique per issuer; only the leaf's is matched (the
		// leaf is issued by the pinned operator CA).
		if i == 0 && c.SerialNumber != nil && r.serials[c.SerialNumber.Text(16)] {
			return ErrRevoked
		}
	}

	return nil
}

func parseRevocations(raw []byte) (map[string]bool, map[string]bool, error) {
	serials, digests := map[string]bool{}, map[string]bool{}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	n := 0

	for sc.Scan() {
		n++

		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		kind, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, fmt.Errorf("%w on line %d: want serial:<hex> or sha256:<hex>", errRevocationLine, n)
		}

		val = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(val), ":", ""))

		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "serial":
			s, ok := new(big.Int).SetString(val, 16)
			if !ok || s.Sign() <= 0 {
				return nil, nil, fmt.Errorf("%w on line %d: serial is not a positive hex number", errRevocationLine, n)
			}

			serials[s.Text(16)] = true
		case "sha256":
			b, err := hex.DecodeString(val)
			if err != nil || len(b) != sha256.Size {
				return nil, nil, fmt.Errorf("%w on line %d: sha256 needs 64 hex digits", errRevocationLine, n)
			}

			digests[val] = true
		default:
			return nil, nil, fmt.Errorf("%w on line %d: unknown kind %q", errRevocationLine, n, kind)
		}
	}

	err := sc.Err()
	if err != nil {
		return nil, nil, err
	}

	return serials, digests, nil
}
