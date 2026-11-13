package testkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
)

// Direction identifies one side of a frame relay.
type Direction string

const (
	// ClientToServer carries requests and cancel frames.
	ClientToServer Direction = "client_to_server"
	// ServerToClient carries responses.
	ServerToClient Direction = "server_to_client"
)

// Fault describes one optional mutation applied to a relayed frame.
type Fault struct {
	Delay   time.Duration
	Drop    bool
	Close   bool
	Replace *protocol.Frame
}

// FaultFunc returns an injection action for a frame. Sequence starts at one
// for each direction on each accepted TCP connection.
type FaultFunc func(direction Direction, sequence int, frame protocol.Frame) Fault

// ProxyStats is a point-in-time frame relay snapshot.
type ProxyStats struct {
	AcceptedConnections uint64
	DialFailures        uint64
	Requests            uint64
	Responses           uint64
	DroppedFrames       uint64
	DelayedFrames       uint64
	ReplacedFrames      uint64
	ClosedConnections   uint64
}

// FaultProxy relays framed TCP traffic and optionally delays, drops, replaces,
// or terminates individual connections.
type FaultProxy struct {
	target   string
	policy   FaultFunc
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
	closeOne sync.Once

	acceptedConnections atomic.Uint64
	dialFailures        atomic.Uint64
	requests            atomic.Uint64
	responses           atomic.Uint64
	droppedFrames       atomic.Uint64
	delayedFrames       atomic.Uint64
	replacedFrames      atomic.Uint64
	closedConnections   atomic.Uint64
}

// NewFaultProxy starts a proxy targeting target on a loopback ephemeral port.
func NewFaultProxy(target string, policy FaultFunc) (*FaultProxy, error) {
	if target == "" {
		return nil, errors.New("testkit: proxy target is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &FaultProxy{
		target:   target,
		policy:   policy,
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
	}
	proxy.workers.Add(1)
	go proxy.acceptLoop()
	return proxy, nil
}

// Addr returns the client-facing proxy address.
func (proxy *FaultProxy) Addr() string {
	return proxy.listener.Addr().String()
}

// Stats returns a consistent point-in-time snapshot.
func (proxy *FaultProxy) Stats() ProxyStats {
	return ProxyStats{
		AcceptedConnections: proxy.acceptedConnections.Load(),
		DialFailures:        proxy.dialFailures.Load(),
		Requests:            proxy.requests.Load(),
		Responses:           proxy.responses.Load(),
		DroppedFrames:       proxy.droppedFrames.Load(),
		DelayedFrames:       proxy.delayedFrames.Load(),
		ReplacedFrames:      proxy.replacedFrames.Load(),
		ClosedConnections:   proxy.closedConnections.Load(),
	}
}

// Close stops accepting connections and waits for active relays to exit.
func (proxy *FaultProxy) Close() error {
	var closeErr error
	proxy.closeOne.Do(func() {
		proxy.cancel()
		closeErr = proxy.listener.Close()
		proxy.workers.Wait()
	})
	return closeErr
}

func (proxy *FaultProxy) acceptLoop() {
	defer proxy.workers.Done()
	for {
		conn, err := proxy.listener.Accept()
		if err != nil {
			if proxy.ctx.Err() != nil {
				return
			}
			continue
		}
		proxy.acceptedConnections.Add(1)
		proxy.workers.Add(1)
		go proxy.relay(conn)
	}
}

func (proxy *FaultProxy) relay(downstream net.Conn) {
	defer proxy.workers.Done()
	defer func() { _ = downstream.Close() }()

	dialer := net.Dialer{Timeout: 2 * time.Second}
	upstream, err := dialer.DialContext(proxy.ctx, "tcp", proxy.target)
	if err != nil {
		proxy.dialFailures.Add(1)
		return
	}
	defer func() { _ = upstream.Close() }()

	connectionContext, cancel := context.WithCancel(proxy.ctx)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() {
		_ = proxy.pump(connectionContext, downstream, upstream, ClientToServer)
		done <- struct{}{}
	}()
	go func() {
		_ = proxy.pump(connectionContext, upstream, downstream, ServerToClient)
		done <- struct{}{}
	}()
	<-done
	proxy.closedConnections.Add(1)
	cancel()
	_ = downstream.Close()
	_ = upstream.Close()
	<-done
}

func (proxy *FaultProxy) pump(
	ctx context.Context,
	source net.Conn,
	destination net.Conn,
	direction Direction,
) error {
	sequence := 0
	for {
		frame, err := protocol.DefaultCodec.Read(source)
		if err != nil {
			return err
		}
		switch frame.Type {
		case protocol.FrameTypeRequest:
			sequence++
			proxy.requests.Add(1)
		case protocol.FrameTypeResponse:
			sequence++
			proxy.responses.Add(1)
		}

		current := *frame
		fault := Fault{}
		if proxy.policy != nil {
			fault = proxy.policy(direction, sequence, current)
		}
		if fault.Close {
			return errors.New("testkit: injected connection close")
		}
		if fault.Drop {
			proxy.droppedFrames.Add(1)
			continue
		}
		if fault.Delay > 0 {
			proxy.delayedFrames.Add(1)
			timer := time.NewTimer(fault.Delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			}
		}
		if fault.Replace != nil {
			proxy.replacedFrames.Add(1)
			current = *fault.Replace
		}
		if err := protocol.DefaultCodec.Write(destination, current); err != nil {
			return fmt.Errorf("testkit: relay frame: %w", err)
		}
	}
}
