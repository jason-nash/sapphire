package proxy

import (
	"context"
	"io"
	"net"
	"sync"
)

type Dial func(context.Context) (net.Conn, error)

// Relay preserves TCP half-closes (Docker uses upgraded, bidirectional streams).
func Relay(ctx context.Context, a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	stop := context.AfterFunc(ctx, func() {
		a.Close()
		b.Close()
	})
	defer stop()
	var copies sync.WaitGroup
	copyStream := func(dst, src net.Conn) {
		defer copies.Done()
		_, err := io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			half.CloseWrite()
		} else {
			dst.Close()
		}
	}
	copies.Add(2)
	go copyStream(a, b)
	go copyStream(b, a)
	copies.Wait()
}

func Serve(ctx context.Context, listener net.Listener, dial Dial) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	var connections sync.WaitGroup
	defer func() {
		cancel()
		listener.Close()
		connections.Wait()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			remote, err := dial(ctx)
			if err != nil {
				conn.Close()
				return
			}
			Relay(ctx, conn, remote)
		}()
	}
}
