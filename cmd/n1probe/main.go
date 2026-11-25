// Command n1probe drives the control protocol for the Gate N1 checks an
// official Tailscale client cannot express (P09; internal/n1probe):
//
//	n1probe switch-tenant --url U --key-a FILE --key-b FILE
//	n1probe flood --kind enroll|bootstrap|mapstream --url U [--bootstrap-url B]
//	              [--tenant SLUG] --key FILE --rate R --duration D [--max-inflight N]
//
// Keys are read from files ("-" for stdin, one key) and never printed. The
// output is JSON on stdout: one line per switch-tenant case, one object for
// a flood.
//
// Exit status: 0 all cases pass / flood ran; 1 a switch-tenant case leaked
// (an answer to a refused request, or an identifier of the other tenant);
// 2 usage or setup error (no case ran); 3 no leak but an expectation failed
// (a positive control, a refusal or a session close).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/n1probe"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage:
  n1probe switch-tenant --url U --key-a FILE --key-b FILE
  n1probe flood --kind enroll|bootstrap|mapstream --url U [--bootstrap-url B] [--tenant SLUG]
                --key FILE --rate R --duration D [--max-inflight N]`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)

		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	switch args[0] {
	case "switch-tenant":
		return switchTenant(ctx, args[1:], stdin, stdout, stderr)
	case "flood":
		return flood(ctx, args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintln(stderr, usage)

		return 2
	}
}

// readKey reads one key from path ("-": stdin), trimmed. Never printed.
func readKey(path string, stdin io.Reader) (string, error) {
	if path == "" {
		return "", errors.New("key file required")
	}

	var r io.Reader = stdin

	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("opening key file: %w", err)
		}
		defer f.Close()

		r = f
	}

	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading key: %w", err)
	}

	k := strings.TrimSpace(line)
	if k == "" {
		return "", errors.New("empty key")
	}

	return k, nil
}

func switchTenant(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("switch-tenant", flag.ContinueOnError)
	fs.SetOutput(stderr)
	u := fs.String("url", "", "cluster URL (the clients' control URL)")
	ka := fs.String("key-a", "", "file with a reusable pre-auth key of tenant A")
	kb := fs.String("key-b", "", "file with a reusable pre-auth key of tenant B")

	if fs.Parse(args) != nil || *u == "" {
		fmt.Fprintln(stderr, usage)

		return 2
	}

	if *ka == "-" && *kb == "-" {
		fmt.Fprintln(stderr, "only one key can come from stdin")

		return 2
	}

	keyA, err := readKey(*ka, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "key-a:", err)

		return 2
	}

	keyB, err := readKey(*kb, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "key-b:", err)

		return 2
	}

	results, err := n1probe.SwitchTenant(ctx, n1probe.SwitchConfig{URL: strings.TrimRight(*u, "/"), KeyA: keyA, KeyB: keyB})
	if err != nil {
		fmt.Fprintln(stderr, err)

		return 2
	}

	enc := json.NewEncoder(stdout)
	code := 0

	for _, r := range results {
		_ = enc.Encode(r)

		switch {
		case r.Leak:
			code = 1
		case !r.Pass && code == 0:
			code = 3
		}
	}

	return code
}

func flood(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flood", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "enroll | bootstrap | mapstream")
	u := fs.String("url", "", "cluster URL (enroll, mapstream)")
	bu := fs.String("bootstrap-url", "", "bootstrap URL (bootstrap)")
	tenant := fs.String("tenant", "", "tenant slug sent to bootstrap (optional)")
	kf := fs.String("key", "", "file with tenant A's reusable pre-auth key")
	rate := fs.Float64("rate", 10, "operations started per second")
	dur := fs.Duration("duration", 30*time.Second, "stop starting operations after this")
	maxIn := fs.Int("max-inflight", 64, "concurrent operations (streams for mapstream)")

	if fs.Parse(args) != nil || *kind == "" {
		fmt.Fprintln(stderr, usage)

		return 2
	}

	k, err := readKey(*kf, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "key:", err)

		return 2
	}

	res, err := n1probe.Flood(ctx, n1probe.FloodConfig{
		Kind: *kind, URL: strings.TrimRight(*u, "/"), BootstrapURL: *bu, Tenant: *tenant,
		Key: k, Rate: *rate, Duration: *dur, MaxInflight: *maxIn,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)

		return 2
	}

	_ = json.NewEncoder(stdout).Encode(res)

	return 0
}
