package guest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDecodeRequest(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"action":"machine.clone","name":"one","destination":"two"}`))
	if err != nil || req.Destination != "two" {
		t.Fatalf("clone destination: %+v, %v", req, err)
	}
	req, err = DecodeRequest([]byte(`{"action":"machine.create","configuration":{"name":"one"}}`))
	if err != nil || req.Configuration.Distribution != "alpine" || req.Configuration.CPUs != 2 {
		t.Fatalf("defaults: %+v, %v", req, err)
	}
	req, err = DecodeRequest([]byte(`{"action":"machine.create","configuration":{"name":"one","cpus":0}}`))
	if err != nil || req.Configuration.Validate() == nil {
		t.Fatalf("explicit zero must not silently become a default: %v", err)
	}
	for _, input := range []string{
		`{"action":"status","unknown":true}`, `{"action":"status"} {}`,
		`{"action":"machine.create","configuration":{"name":"one","fake":true}}`,
		`{"action":"machine.exec","arguments":["a\u0000b"]}`, `{}`, `[]`, `null`,
	} {
		if _, err := DecodeRequest([]byte(input)); err == nil {
			t.Errorf("accepted invalid request %s", input)
		}
	}
}

func TestWallClockRequiresPlausibleHostTime(t *testing.T) {
	for _, invalid := range []time.Time{time.Unix(0, 0), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if ValidateWallClock(invalid) == nil {
			t.Fatalf("accepted invalid guest clock %s", invalid)
		}
	}
	if err := ValidateWallClock(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationLimits(t *testing.T) {
	config := MachineConfiguration{Name: "linux-1"}
	config.Defaults()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../linux", "A", "-a", "", "docker", strings.Repeat("a", 64)} {
		if ValidateName(name) == nil {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	for _, change := range []func(*MachineConfiguration){
		func(c *MachineConfiguration) { c.Rosetta = true },
		func(c *MachineConfiguration) { c.ShareHome = true },
		func(c *MachineConfiguration) { c.Docker = true },
		func(c *MachineConfiguration) { c.CPUs = 65 },
		func(c *MachineConfiguration) { c.MemoryGiB = -1 },
		func(c *MachineConfiguration) { c.DiskGiB = 7 },
		func(c *MachineConfiguration) { c.Distribution = "pretend-linux" },
	} {
		copy := config
		change(&copy)
		if copy.Validate() == nil {
			t.Errorf("accepted unsupported configuration %+v", copy)
		}
	}
}

func TestProcIdentityParsing(t *testing.T) {
	input := "123 (a ) tricky (name)) S " + strings.Repeat("0 ", 18) + "123456 0 0"
	start, state, err := ParseProcStat(input)
	if err != nil || start != 123456 || state != 'S' {
		t.Fatalf("start=%d state=%c error=%v", start, state, err)
	}
	for _, invalid := range []string{"", "123 (a) S", "123 name S", "123 (a) S " + strings.Repeat("x ", 20)} {
		if _, _, err := ParseProcStat(invalid); err == nil {
			t.Errorf("accepted malformed /proc/stat %q", invalid)
		}
	}
}

func TestMemoryAndOutputLimits(t *testing.T) {
	values := ParseMeminfo("MemTotal: 1024 kB\nMemAvailable: 512 kB\ninvalid:\nOther: x kB")
	if values["MemTotal"] != 1048576 || values["MemAvailable"] != 524288 {
		t.Fatal(values)
	}
	var output limitedBuffer
	input := bytes.Repeat([]byte("x"), MaxOutputSize)
	if n, err := output.Write(input); err != nil || n != len(input) || output.truncated {
		t.Fatal("exact capacity should not be marked truncated")
	}
	output.Write([]byte("extra"))
	if !output.truncated || len(output.data) != MaxOutputSize {
		t.Fatal("output must drain while preserving its size limit")
	}
}

func TestBlankDiskRequiresEveryByte(t *testing.T) {
	if err := checkZeroReader(bytes.NewReader(make([]byte, 4096)), 4096); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, (1<<20)+17)
	data[len(data)-1] = 1
	if err := checkZeroReader(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("nonzero last byte accepted")
	}
	if err := checkZeroReader(bytes.NewReader([]byte{0}), 2); err == nil {
		t.Fatal("short block device read accepted")
	}
}
