package daemon

import (
	"fmt"
	"io"
	"os"
	"strings"

	"sapphire/engine/internal/privatefs"
)

const diagnosticBytes = 8 << 10

type bootLog struct {
	path   string
	offset int64
	lines  int
}

func (l bootLog) tail() string {
	file, err := privatefs.Open(l.path, os.O_RDONLY)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= l.offset {
		return ""
	}
	start := max(l.offset, info.Size()-diagnosticBytes)
	data := make([]byte, info.Size()-start)
	n, err := file.ReadAt(data, start)
	if err != nil && err != io.EOF {
		return ""
	}
	data = data[:n]
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > l.lines {
		lines = lines[len(lines)-l.lines:]
	}
	return strings.Join(lines, "\n")
}

func (p *child) diagnosticError(cause error) error {
	var details strings.Builder
	for _, log := range p.logs {
		fmt.Fprintf(&details, "\nsee %s", log.path)
		if tail := log.tail(); tail != "" {
			fmt.Fprintf(&details, "\n--- %s (current launch tail) ---\n%s", log.path, tail)
		}
	}
	return fmt.Errorf("%w%s", cause, details.String())
}
