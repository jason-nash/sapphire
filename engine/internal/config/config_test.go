package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndValidation(t *testing.T) {
	t.Setenv("SAPPHIRE_HOME", "/tmp/sapphire-test")
	t.Setenv("SAPPHIRE_VMM", "")
	t.Setenv("SAPPHIRE_MAC", "")
	t.Setenv("SAPPHIRE_SUBNET", "")
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.CPUs != 4 || c.MemoryMiB != 4096 || c.DataGiB != 64 || c.MAC != "5a:94:ef:e4:0c:ee" || c.StopTimeout != 45*time.Second {
		t.Fatalf("incorrect defaults: %+v", c)
	}
	gateway, guest := c.NetworkAddresses()
	if gateway != "192.168.128.1" || guest != "192.168.128.2" {
		t.Fatalf("addresses: %s %s", gateway, guest)
	}
	for name, mutate := range map[string]func(*Config){
		"cpu":         func(c *Config) { c.CPUs = 0 },
		"memory":      func(c *Config) { c.MemoryMiB = 1 },
		"zero disk":   func(c *Config) { c.DataGiB = 0 },
		"huge disk":   func(c *Config) { c.DataGiB = 2049 },
		"subnet":      func(c *Config) { c.Subnet = "192.168.128.2/24" },
		"public":      func(c *Config) { c.Subnet = "8.8.8.0/24" },
		"ipv6":        func(c *Config) { c.Subnet = "fd00::/24" },
		"multicast":   func(c *Config) { c.MAC = "ff:ff:ff:ff:ff:ff" },
		"zero mac":    func(c *Config) { c.MAC = "00:00:00:00:00:00" },
		"socket path": func(c *Config) { c.Home = "/tmp/" + strings.Repeat("s", 100) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := c
			mutate(&copy)
			if copy.Validate() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestResolveDriver(t *testing.T) {
	dir := t.TempDir()
	sibling := filepath.Join(dir, "sapphire-vmm")
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := Config{}
	got, err := c.ResolveDriver(filepath.Join(dir, "sapphire-engine"))
	if err != nil || got != sibling {
		t.Fatalf("sibling resolution: %q %v", got, err)
	}
	c.Driver = filepath.Join(dir, "missing")
	if _, err := c.ResolveDriver(filepath.Join(dir, "sapphire-engine")); err == nil {
		t.Fatal("explicit driver failure must not silently fall back")
	}
}
