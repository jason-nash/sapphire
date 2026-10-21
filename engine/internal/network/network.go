package network

import (
	"context"
	"errors"
	"io"
	"net"
	"os"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"golang.org/x/sys/unix"

	"sapphire/engine/internal/config"
)

const (
	EthernetHeader = 14
	MTU            = 1500
	MaxFrame       = MTU + EthernetHeader + 4
)

// FD 3 is SOCK_DGRAM, one complete Ethernet frame per datagram, no prefix.
func SocketPair() (*net.UnixConn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	host := os.NewFile(uintptr(fds[0]), "ethernet-host")
	guest := os.NewFile(uintptr(fds[1]), "ethernet-vmm")
	conn, err := net.FileConn(host)
	host.Close()
	if err != nil {
		guest.Close()
		return nil, nil, err
	}
	return conn.(*net.UnixConn), guest, nil
}

// Filtering also prevents short datagrams from reaching gVisor's Ethernet parser.
type Frames struct{ *net.UnixConn }

func (c Frames) Read(p []byte) (int, error) {
	var frame [MaxFrame + 1]byte
	for {
		n, _, flags, _, err := c.ReadMsgUnix(frame[:], nil)
		if err != nil {
			return 0, err
		}
		if flags&unix.MSG_TRUNC != 0 || n < EthernetHeader || n > MaxFrame {
			continue
		}
		if len(p) < n {
			return 0, io.ErrShortBuffer
		}
		return copy(p, frame[:n]), nil
	}
}

func (c Frames) Write(p []byte) (int, error) {
	if len(p) < EthernetHeader || len(p) > MaxFrame {
		return 0, errors.New("invalid Ethernet frame size")
	}
	return c.UnixConn.Write(p)
}

func Configuration(c config.Config) *types.Configuration {
	gateway, guest := c.NetworkAddresses()
	return &types.Configuration{
		MTU: MTU, Subnet: c.Subnet, GatewayIP: gateway,
		GatewayMacAddress: "5a:94:ef:e4:0c:01",
		DHCPStaticLeases:  map[string]string{guest: c.MAC},
		Protocol:          types.VfkitProtocol,
		DNS: []types.Zone{{
			Name: "sapphire.internal.", Records: []types.Record{
				{Name: "gateway", IP: net.ParseIP(gateway)},
				{Name: "guest", IP: net.ParseIP(guest)},
			},
		}},
	}
}

type Stack struct {
	network *virtualnetwork.VirtualNetwork
	conn    *net.UnixConn
}

func New(c config.Config, conn *net.UnixConn) (*Stack, error) {
	network, err := virtualnetwork.New(Configuration(c))
	if err != nil {
		return nil, err
	}
	return &Stack{network: network, conn: conn}, nil
}

func (s *Stack) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { s.conn.Close() })
	defer stop()
	return s.network.AcceptVfkit(ctx, Frames{s.conn})
}

func (s *Stack) Close() error {
	s.conn.Close()
	// The library closes its published forwards here. Its DNS/DHCP stack lives
	// until process exit; this daemon creates exactly one stack per lifetime.
	return s.network.Close()
}
