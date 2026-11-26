package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// manifest is the load host's half of the reproducibility manifest of a
// cell (evidence/P14-benchmark-plan.md §6): what ran (commit, binary
// sha256, build info, flags) and where (host size and limits). The server
// half comes from server.sh manifest. Flags name files, never key
// contents, so the manifest holds no secret.
type manifest struct {
	Tool      string            `json:"tool"`
	Label     string            `json:"label,omitempty"`
	Commit    string            `json:"commit,omitempty"`
	Args      []string          `json:"args"`
	Binary    string            `json:"binary_sha256"`
	GoVersion string            `json:"go_version"`
	Build     map[string]string `json:"build_settings,omitempty"`
	Start     string            `json:"start_utc"`
	End       string            `json:"end_utc,omitempty"`
	Host      hostInfo          `json:"host"`
}

type hostInfo struct {
	Hostname       string `json:"hostname"`
	CPUs           int    `json:"cpus"`
	CPUModel       string `json:"cpu_model,omitempty"`
	MemTotal       string `json:"mem_total,omitempty"`
	Kernel         string `json:"kernel,omitempty"`
	OpenFiles      string `json:"max_open_files,omitempty"`
	LocalPortRange string `json:"ip_local_port_range,omitempty"`
}

func newManifest(cfg config, args []string) *manifest {
	m := &manifest{
		Tool: "capload", Label: cfg.label, Commit: cfg.commit, Args: args,
		GoVersion: runtime.Version(), Start: time.Now().UTC().Format(time.RFC3339),
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		m.Build = map[string]string{}
		for _, s := range bi.Settings {
			m.Build[s.Key] = s.Value
		}
	}

	if exe, err := os.Executable(); err == nil { //nolint:noinlineerr
		m.Binary = fileSHA256(exe)
	}

	m.Host = hostInfo{
		CPUs:           runtime.NumCPU(),
		CPUModel:       procField("/proc/cpuinfo", "model name"),
		MemTotal:       procField("/proc/meminfo", "MemTotal"),
		Kernel:         readTrim("/proc/sys/kernel/osrelease"),
		OpenFiles:      procField("/proc/self/limits", "Max open files"),
		LocalPortRange: strings.Join(strings.Fields(readTrim("/proc/sys/net/ipv4/ip_local_port_range")), "-"),
	}
	m.Host.Hostname, _ = os.Hostname()

	return m
}

func (m *manifest) finish(dir string) error {
	m.End = time.Now().UTC().Format(time.RFC3339)

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "manifest-load.json"), b, 0o600)
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()

	_, err = io.Copy(h, f)
	if err != nil {
		return ""
	}

	return hex.EncodeToString(h.Sum(nil))
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(b))
}

// procField returns the value of the first line of path starting with key
// (/proc/cpuinfo "key : value", /proc/meminfo "key: value", /proc/self/limits
// "Max open files  soft hard unit").
func procField(path, key string) string {
	for line := range strings.SplitSeq(readTrim(path), "\n") {
		if rest, ok := strings.CutPrefix(line, key); ok {
			return strings.Join(strings.Fields(strings.TrimLeft(rest, " \t:")), " ")
		}
	}

	return ""
}
