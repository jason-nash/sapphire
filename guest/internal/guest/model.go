package guest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Version         = "0.1.0"
	Protocol        = 1
	RPCPort         = 1024
	DockerPort      = 1025
	ForwardPort     = 1026
	MaxRequestSize  = 1 << 20
	MaxOutputSize   = 256 << 10
	MaxResponseSize = 4 << 20
	MachinesPath    = "/var/lib/sapphire/machines"
	CgroupPath      = "/sys/fs/cgroup/sapphire"
)

type Request struct {
	Action        string                `json:"action"`
	Name          string                `json:"name,omitempty"`
	Destination   string                `json:"destination,omitempty"`
	Configuration *MachineConfiguration `json:"configuration,omitempty"`
	Arguments     []string              `json:"arguments,omitempty"`
}

type Response struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result"`
	Error  string `json:"error"`
}

type MachineConfiguration struct {
	Name         string `json:"name"`
	Distribution string `json:"distribution"`
	CPUs         int    `json:"cpus"`
	MemoryGiB    int    `json:"memoryGiB"`
	DiskGiB      int    `json:"diskGiB"`
	Docker       bool   `json:"docker"`
	Rosetta      bool   `json:"rosetta"`
	ShareHome    bool   `json:"shareHome"`
}

func (c *MachineConfiguration) UnmarshalJSON(data []byte) error {
	type configuration MachineConfiguration
	value := configuration{Distribution: "alpine", CPUs: 2, MemoryGiB: 2, DiskGiB: 16}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*c = MachineConfiguration(value)
	return nil
}

// These are official, immutable Linux/arm64 image manifests, not moving tags.
var distributionImages = map[string]string{
	"ubuntu": "docker.io/library/ubuntu@sha256:18b33ff3829707f31ada654a963b9ee35f2e6f0241b964bd17a22a9b330cb63d",
	"debian": "docker.io/library/debian@sha256:0daf3052d9359adaa0de63035b87949cc6c7966bfd6d60746d318ee1999f5edb",
	"fedora": "docker.io/library/fedora@sha256:8de9a064b0d0371271a1ba30980563cb92af15e3be2a94b56d2b19fa215a4a45",
	"alpine": "docker.io/library/alpine@sha256:ae959799024a09da597284763c417a6e820e020fad0634c5267cf4f125e682bf",
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func ValidateName(name string) error {
	if !namePattern.MatchString(name) || name == "docker" {
		return errors.New("machine name must be 1-63 lowercase letters, digits, underscores or hyphens, start with a letter or digit, and cannot be 'docker'")
	}
	return nil
}

func (c *MachineConfiguration) Defaults() {
	if c.Distribution == "" {
		c.Distribution = "alpine"
	}
	if c.CPUs == 0 {
		c.CPUs = 2
	}
	if c.MemoryGiB == 0 {
		c.MemoryGiB = 2
	}
	if c.DiskGiB == 0 {
		c.DiskGiB = 16
	}
}

func (c MachineConfiguration) Validate() error {
	if err := ValidateName(c.Name); err != nil {
		return err
	}
	if _, ok := distributionImages[c.Distribution]; !ok {
		return errors.New("distribution must be ubuntu, debian, fedora, or alpine")
	}
	if c.CPUs < 1 || c.CPUs > 64 || c.MemoryGiB < 1 || c.MemoryGiB > 256 || c.DiskGiB < 8 || c.DiskGiB > 2048 {
		return errors.New("use 1-64 CPUs, 1-256 GiB memory, and 8-2048 GiB disk")
	}
	if c.Docker {
		return errors.New("machines are Linux namespaces; use docker.start for the shared guest Docker engine")
	}
	if c.Rosetta {
		return errors.New("rosetta is unsupported: this guest runs native Linux/arm64")
	}
	if c.ShareHome {
		return errors.New("shareHome is unsupported: no host filesystem transport is configured")
	}
	return nil
}

func DecodeRequest(data []byte) (Request, error) {
	var req Request
	if len(data) > MaxRequestSize {
		return req, errors.New("request exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid request: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return req, errors.New("expected one JSON object per line")
	}
	if req.Action == "" {
		return req, errors.New("action is required")
	}
	if len(req.Arguments) > 256 {
		return req, errors.New("too many command arguments")
	}
	for _, arg := range req.Arguments {
		if strings.ContainsRune(arg, 0) {
			return req, errors.New("arguments cannot contain NUL")
		}
	}
	return req, nil
}

type ProcessIdentity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
	BootID     string `json:"bootID"`
}

type MachineState struct {
	Configuration MachineConfiguration `json:"configuration"`
	Process       ProcessIdentity      `json:"process"`
	CreatedAt     time.Time            `json:"createdAt"`
	Source        string               `json:"source"`
	InitSystem    string               `json:"initSystem"`
	Booting       bool                 `json:"booting,omitempty"`
}

type MachineInfo struct {
	Name          string               `json:"name"`
	Status        string               `json:"status"`
	Arch          string               `json:"arch"`
	CPUs          int                  `json:"cpus"`
	Memory        uint64               `json:"memory"`
	Disk          uint64               `json:"disk"`
	Dir           string               `json:"dir"`
	SSHLocalPort  int                  `json:"sshLocalPort"`
	Configuration MachineConfiguration `json:"configuration"`
	Process       *ProcessIdentity     `json:"process,omitempty"`
	InitSystem    string               `json:"initSystem"`
	Source        string               `json:"source"`
	CreatedAt     time.Time            `json:"createdAt"`
	Warning       string               `json:"warning,omitempty"`
}

type CommandResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated"`
}

func ValidateWallClock(now time.Time) error {
	if now.Before(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || now.Year() >= 2100 {
		return fmt.Errorf("guest wall clock is %s; a current host-backed rtc0 is required before Docker or Kubernetes starts", now.UTC().Format(time.RFC3339))
	}
	return nil
}

// comm (field 2) can contain whitespace and parentheses. Field 22 is starttime.
func ParseProcStat(text string) (uint64, byte, error) {
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, 0, errors.New("invalid proc stat: missing comm")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, errors.New("invalid proc stat: missing fields")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid proc starttime: %w", err)
	}
	return start, fields[0][0], nil
}

func ParseMeminfo(text string) map[string]uint64 {
	values := make(map[string]uint64)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 2 && fields[2] == "kB" {
			n *= 1024
		}
		values[strings.TrimSuffix(fields[0], ":")] = n
	}
	return values
}

type limitedBuffer struct {
	data      []byte
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxOutputSize - len(b.data)
	if n > remaining {
		b.truncated = true
	}
	if remaining > 0 {
		b.data = append(b.data, p[:min(remaining, n)]...)
	}
	return n, nil
}
