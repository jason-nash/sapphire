package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"sapphire/engine/internal/config"
)

func pair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	host, guestFile, err := SocketPair()
	if err != nil {
		t.Fatal(err)
	}
	guest, err := net.FileConn(guestFile)
	guestFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(); guest.Close() })
	return host, guest.(*net.UnixConn)
}

func TestDatagramFramesHaveNoStreamPrefix(t *testing.T) {
	host, guest := pair(t)
	host.SetDeadline(time.Now().Add(time.Second))
	guest.SetDeadline(time.Now().Add(time.Second))
	adapter := Frames{host}
	frame := bytes.Repeat([]byte{0x5a}, 60)
	guest.Write([]byte{1, 2, 3})
	guest.Write(make([]byte, MaxFrame+100))
	guest.Write(frame)
	buffer := make([]byte, 65536)
	n, err := adapter.Read(buffer)
	if err != nil || !bytes.Equal(buffer[:n], frame) {
		t.Fatalf("raw receive: n=%d err=%v", n, err)
	}
	if _, err := adapter.Write(frame); err != nil {
		t.Fatal(err)
	}
	n, err = guest.Read(buffer)
	if err != nil || !bytes.Equal(buffer[:n], frame) {
		t.Fatalf("unexpected stream prefix: %x, %v", buffer[:n], err)
	}
	if _, err := adapter.Write([]byte{1}); err == nil {
		t.Fatal("accepted short Ethernet frame")
	}
}

func TestVirtualNetworkAnswersRawEthernetARP(t *testing.T) {
	host, guest := pair(t)
	c, _ := config.Default()
	c.Home = "/tmp/sapphire-test"
	c.Subnet, c.MAC = "192.168.128.0/24", "5a:94:ef:e4:0c:ee"
	stack, err := New(c, host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer stack.Close()
	done := make(chan error, 1)
	go func() { done <- stack.Run(ctx) }()
	mac, _ := net.ParseMAC(c.MAC)
	frame := make([]byte, 42)
	copy(frame[:6], []byte{255, 255, 255, 255, 255, 255})
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], 0x0806)
	copy(frame[14:22], []byte{0, 1, 8, 0, 6, 4, 0, 1})
	copy(frame[22:28], mac)
	copy(frame[28:32], net.ParseIP("192.168.128.2").To4())
	copy(frame[38:42], net.ParseIP("192.168.128.1").To4())
	guest.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := guest.Write(frame); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2048)
	n, err := guest.Read(reply)
	if err != nil {
		t.Fatal(err)
	}
	if n < 42 || binary.BigEndian.Uint16(reply[20:22]) != 2 ||
		!bytes.Equal(reply[:6], mac) ||
		!bytes.Equal(reply[28:32], net.ParseIP("192.168.128.1").To4()) {
		t.Fatalf("invalid ARP response: %x", reply[:n])
	}
	cancel()
	host.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Ethernet loop did not unblock on cancellation")
	}
}
