// Command mesh-rebootstrap re-points the clients of a moved tailnet at its
// new owner cluster (P12b, owner decision Q5): it asks bootstrap for the
// control URL with the tailnet's reusable pre-auth key (key id + proof only),
// then runs a command per target, at most -concurrency at a time, and writes
// per-target timings and t50/t90/t100 as JSON.
//
//	mesh-rebootstrap -bootstrap https://boot.example:8444 -authkey-file key \
//	  -targets hosts.txt -concurrency 8 \
//	  -cmd 'ssh {target} sudo tailscale up --login-server {login_server} --force-reauth --auth-key=file:/etc/mesh/authkey'
//
// The command gets {target} and {login_server} (also as MESH_TARGET and
// MESH_LOGIN_SERVER in its environment), never the key: put the key where
// the client reads it (`--auth-key=file:`). Output is kept redacted.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/zhaduchanhzz/mesh-controlplane/internal/rebootstrap"
)

var (
	errUsage      = errors.New("usage")
	errBadURL     = errors.New("bootstrap returned an unusable control URL")
	errFailed     = errors.New("targets failed")
	errBadTargets = errors.New("bad targets file")
	errBadCA      = errors.New("no certificate in the CA file")

	targetPattern = regexp.MustCompile(`^[A-Za-z0-9._@:-]{1,253}$`)
	loginPattern  = regexp.MustCompile(`^https://[A-Za-z0-9.-]{1,253}(:[0-9]{1,5})?/?$`)
)

func main() {
	var (
		boot        = flag.String("bootstrap", "", "bootstrap base URL (https://host:port)")
		keyFile     = flag.String("authkey-file", "", "file holding the tailnet's reusable pre-auth key")
		tenant      = flag.String("tenant", "", "optional tailnet slug the key must belong to")
		caFile      = flag.String("ca", "", "optional PEM CA bundle for bootstrap (default: system roots)")
		targetsFile = flag.String("targets", "", "file with one target per line (# comments)")
		cmdTmpl     = flag.String("cmd", "", "command per target, run with sh -c; {target} and {login_server} are replaced")
		concurrency = flag.Int("concurrency", 8, "targets re-bootstrapped at once")
		perTarget   = flag.Duration("timeout", 3*time.Minute, "time limit per target")
		maxWait     = flag.Duration("max-wait", 10*time.Minute, "how long to wait while the tailnet is MOVING")
		out         = flag.String("out", "", "write the JSON summary here (default: stdout)")
		dryRun      = flag.Bool("dry-run", false, "resolve and print the commands, run nothing")
		fromURL     = flag.String("from", "", "control URL before the move: wait until bootstrap gives another one")
		poll        = flag.Duration("poll", 2*time.Second, "longest wait between bootstrap polls")
	)

	flag.Parse()

	opts := options{
		boot: *boot, keyFile: *keyFile, tenant: *tenant, caFile: *caFile, targetsFile: *targetsFile, cmdTmpl: *cmdTmpl,
		concurrency: *concurrency, perTarget: *perTarget, maxWait: *maxWait, out: *out, dryRun: *dryRun,
		fromURL: *fromURL, poll: *poll,
	}

	if err := run(opts); err != nil { //nolint:noinlineerr
		log.Fatalf("mesh-rebootstrap: %v", err)
	}
}

type options struct {
	boot, keyFile, tenant, caFile, targetsFile, cmdTmpl string
	concurrency                                         int
	perTarget, maxWait, poll                            time.Duration
	out, fromURL                                        string
	dryRun                                              bool
}

func run(o options) error {
	boot, keyFile, tenant, caFile, targetsFile, cmdTmpl := o.boot, o.keyFile, o.tenant, o.caFile, o.targetsFile, o.cmdTmpl
	concurrency, perTarget, out := o.concurrency, o.perTarget, o.out

	if boot == "" || keyFile == "" || targetsFile == "" || cmdTmpl == "" {
		return fmt.Errorf("%w: -bootstrap, -authkey-file, -targets and -cmd are required", errUsage)
	}

	if !strings.Contains(cmdTmpl, "{login_server}") {
		return fmt.Errorf("%w: -cmd must use {login_server}", errUsage)
	}

	targets, err := readTargets(targetsFile)
	if err != nil {
		return err
	}

	key, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("reading the key file: %w", err)
	}

	cred, err := rebootstrap.CredentialOf(string(key))
	clear(key)

	if err != nil {
		return err
	}

	client, err := httpClient(caFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	r := &rebootstrap.Resolver{
		BaseURL: boot, Tenant: tenant, Client: client, MaxWait: o.maxWait, Logf: log.Printf,
		NotURL: o.fromURL, Poll: o.poll,
	}

	t0 := time.Now()

	login, err := r.Resolve(ctx, cred)
	if err != nil {
		return err
	}

	// The URL goes into a shell command: only scheme, host and port.
	if !loginPattern.MatchString(login) {
		return errBadURL
	}

	log.Printf("bootstrap: control URL %s after %s; %d targets, %d at a time", login, time.Since(t0).Round(time.Millisecond), len(targets), concurrency)

	if o.dryRun {
		for _, t := range targets {
			fmt.Println(expand(cmdTmpl, t, login))
		}

		return nil
	}

	runner := func(ctx context.Context, target, login string) ([]byte, error) {
		// Running the operator's own -cmd through a shell is the purpose of
		// the tool; the only values put into it are a target name and the
		// control URL, both checked against host-like patterns above.
		c := exec.CommandContext(ctx, "sh", "-c", expand(cmdTmpl, target, login)) //nolint:gosec // G204: operator-supplied command template, inputs validated (see above)
		c.Env = append(os.Environ(), "MESH_TARGET="+target, "MESH_LOGIN_SERVER="+login)
		c.WaitDelay = 5 * time.Second

		return c.CombinedOutput()
	}

	sum := rebootstrap.Run(ctx, login, targets, concurrency, perTarget, runner)
	log.Printf("done: %d ok, %d failed; t50=%dms t90=%dms t100=%dms", sum.OK, sum.Failed, sum.T50MS, sum.T90MS, sum.T100MS)

	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}

	if out == "" {
		fmt.Println(string(b))
	} else if err := os.WriteFile(out, append(b, '\n'), 0o600); err != nil { //nolint:noinlineerr,gosec // G703: -out is the operator's own output path on its own machine
		return err
	}

	if sum.Failed > 0 {
		return fmt.Errorf("%w: %d of %d", errFailed, sum.Failed, sum.Targets)
	}

	return nil
}

func expand(tmpl, target, login string) string {
	return strings.NewReplacer("{target}", target, "{login_server}", login).Replace(tmpl)
}

// readTargets reads one target per line; names are restricted to host-like
// characters, so they cannot inject shell syntax into the command.
func readTargets(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading targets: %w", err)
	}

	var out []string

	seen := map[string]bool{}

	for i, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}

		if !targetPattern.MatchString(t) {
			return nil, fmt.Errorf("%w: line %d is not a host-like name", errBadTargets, i+1)
		}

		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no targets", errBadTargets)
	}

	return out, nil
}

func httpClient(caFile string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errBadCA
		}

		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	return &http.Client{Timeout: 15 * time.Second, Transport: tr}, nil
}
