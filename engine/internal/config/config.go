package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

const GiB int64 = 1 << 30

type Config struct {
	Home           string
	Driver         string
	CPUs           int
	MemoryMiB      int
	DataGiB        int64
	Subnet         string
	MAC            string
	StartupTimeout time.Duration
	StopTimeout    time.Duration
}

func Default() (Config, error) {
	home := os.Getenv("SAPPHIRE_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Config{}, err
		}
		home = filepath.Join(userHome, ".sapphire")
	}
	c := Config{
		Home: home, Driver: os.Getenv("SAPPHIRE_VMM"),
		CPUs: 4, MemoryMiB: 4096, DataGiB: 64,
		Subnet: "192.168.128.0/24", MAC: "5a:94:ef:e4:0c:ee",
		StartupTimeout: 90 * time.Second, StopTimeout: 45 * time.Second,
	}
	if value := os.Getenv("SAPPHIRE_SUBNET"); value != "" {
		c.Subnet = value
	}
	if value := os.Getenv("SAPPHIRE_MAC"); value != "" {
		c.MAC = value
	}
	return c, nil
}

func (c *Config) Validate() error {
	if c.Home == "" {
		return errors.New("home must not be empty")
	}
	absolute, err := filepath.Abs(c.Home)
	if err != nil {
		return err
	}
	c.Home = absolute
	if c.CPUs < 1 || c.CPUs > 64 {
		return errors.New("cpus must be between 1 and 64")
	}
	if c.MemoryMiB < 256 || c.MemoryMiB > 262144 {
		return errors.New("memory-mib must be between 256 and 262144")
	}
	if c.DataGiB < 1 || c.DataGiB > 2048 {
		return errors.New("data-gib must be between 1 and 2048")
	}
	subnet, err := netip.ParsePrefix(c.Subnet)
	if err != nil || !subnet.Addr().Is4() || subnet.Bits() != 24 || subnet != subnet.Masked() || !subnet.Addr().IsPrivate() {
		return errors.New("subnet must be a private IPv4 /24 network")
	}
	mac, err := net.ParseMAC(c.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return errors.New("mac must be a nonzero unicast Ethernet address")
	}
	c.MAC = mac.String()
	if c.MAC != "5a:94:ef:e4:0c:ee" {
		return errors.New("native driver currently requires MAC 5a:94:ef:e4:0c:ee")
	}
	if c.StartupTimeout <= 0 || c.StartupTimeout > 10*time.Minute || c.StopTimeout <= 0 || c.StopTimeout > time.Minute {
		return errors.New("startup-timeout must be in (0,10m] and stop-timeout in (0,1m]")
	}
	// Darwin's sockaddr_un has a 104-byte path, including its terminator.
	for _, name := range []string{"control.sock", "docker.sock", "vsock.sock"} {
		if len(c.Path(name)) >= 104 {
			return fmt.Errorf("home is too long for Unix socket %s", name)
		}
	}
	return nil
}

func (c Config) Path(name string) string { return filepath.Join(c.Home, name) }

func (c Config) NetworkAddresses() (gateway, guest string) {
	subnet := netip.MustParsePrefix(c.Subnet)
	first := subnet.Addr().Next()
	return first.String(), first.Next().String()
}

func (c Config) ResolveDriver(executable string) (string, error) {
	if c.Driver != "" {
		return executableFile(c.Driver)
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executable), "sapphire-vmm"),
		filepath.Join(filepath.Dir(executable), "..", "native", "build", "sapphire-vmm"),
		filepath.Join(filepath.Dir(executable), "..", "..", "native", "build", "sapphire-vmm"),
		filepath.Join("native", "build", "sapphire-vmm"),
		filepath.Join("..", "native", "build", "sapphire-vmm"),
	}
	for _, candidate := range candidates {
		if path, err := executableFile(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("sapphire-vmm was not found; set SAPPHIRE_VMM or install it beside sapphire-engine")
}

func executableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("VMM %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("VMM %s is not an executable regular file", path)
	}
	return filepath.Abs(path)
}
