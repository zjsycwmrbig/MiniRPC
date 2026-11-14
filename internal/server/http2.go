package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/zjsycwmrbig/mini-rpc/internal/protocol"
	"github.com/zjsycwmrbig/mini-rpc/internal/transport"
	"github.com/zjsycwmrbig/mini-rpc/pkg/rpc"
	"golang.org/x/net/http2"
)

const maxHTTP2EncodedFrameSize = int64(protocol.FixedHeaderSize) +
	int64(protocol.DefaultMaxHeaderSize) +
	int64(protocol.DefaultMaxBodySize)

func (server *Server) serveHTTP2Connection(state *connection) {
	defer server.workers.Done()
	defer func() {
		state.cancelAll()
		_ = state.netConn.Close()
		server.mu.Lock()
		delete(server.connections, state)
		server.observeConnectionsLocked()
		server.mu.Unlock()
	}()
	if !server.completeTLSHandshake(state) {
		return
	}

	protocolServer := &http2.Server{
		MaxConcurrentStreams: uint32(server.config.MaxConcurrentRequests),
	}
	httpServer := &http.Server{
		Handler:        http.HandlerFunc(server.handleHTTP2Stream),
		MaxHeaderBytes: int(protocol.DefaultMaxHeaderSize),
		BaseContext: func(_ net.Listener) context.Context {
			return context.Background()
		},
	}
	if err := http2.ConfigureServer(httpServer, protocolServer); err != nil {
		return
	}
	state.gracefulShutdown = httpServer.Shutdown
	protocolServer.ServeConn(state.netConn, &http2.ServeConnOpts{
		BaseConfig: httpServer,
		Handler:    httpServer.Handler,
	})
}

func (server *Server) handleHTTP2Stream(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path != transport.HTTP2InvokePath {
		http.NotFound(writer, request)
		return
	}
	if !isHTTP2FrameContentType(request.Header.Get("Content-Type")) {
		http.Error(writer, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	limitedBody := http.MaxBytesReader(writer, request.Body, maxHTTP2EncodedFrameSize)
	frame, err := protocol.DefaultCodec.Read(limitedBody)
	if err != nil {
		server.recordHTTP2Rejected()
		http.Error(writer, "invalid Mini-RPC frame", http.StatusBadRequest)
		return
	}
	var trailing [1]byte
	if read, trailingErr := limitedBody.Read(trailing[:]); read != 0 || !errors.Is(trailingErr, io.EOF) {
		server.recordHTTP2Rejected()
		http.Error(writer, "multiple or trailing Mini-RPC frames", http.StatusBadRequest)
		return
	}
	if frame.Type != protocol.FrameTypeRequest {
		server.recordHTTP2Rejected()
		http.Error(writer, "HTTP/2 RPC body must contain a request frame", http.StatusBadRequest)
		return
	}

	if rejection := server.admitHTTP2Request(frame); rejection != nil {
		server.writeHTTP2Frame(writer, rejection)
		return
	}
	state := &connection{requests: make(map[uint64]*requestState)}
	state.prepareRequest(frame.RequestID)
	defer state.releaseRequest(frame.RequestID)
	defer server.finishRequest()

	response := server.dispatchWithContext(request.Context(), state, frame)
	server.writeHTTP2Frame(writer, response)
}

func (server *Server) admitHTTP2Request(request *protocol.Frame) *protocol.Frame {
	server.mu.Lock()
	if server.draining || server.closed {
		server.stats.RejectedRequests++
		server.mu.Unlock()
		server.recordImmediateRejection(
			request,
			rpc.CodeUnavailable,
			"server is shutting down",
		)
		return rpcErrorFrame(request.RequestID, rpc.CodeUnavailable, "server is shutting down")
	}
	if server.activeRequests >= server.config.MaxConcurrentRequests {
		server.stats.ConcurrencyRejected++
		server.mu.Unlock()
		server.recordImmediateRejection(
			request,
			rpc.CodeResourceExhausted,
			"server concurrency limit exceeded",
		)
		return rpcErrorFrame(
			request.RequestID,
			rpc.CodeResourceExhausted,
			"server concurrency limit exceeded",
		)
	}
	if server.activeRequests == 0 {
		server.requestsDone = make(chan struct{})
	}
	server.activeRequests++
	server.mu.Unlock()
	return nil
}

func (server *Server) writeHTTP2Frame(writer http.ResponseWriter, frame *protocol.Frame) {
	if frame == nil {
		http.Error(writer, "missing Mini-RPC response", http.StatusInternalServerError)
		return
	}
	encoded, err := protocol.DefaultCodec.Encode(*frame)
	if err != nil {
		http.Error(writer, "encode Mini-RPC response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	writer.Header().Set("Content-Type", transport.HTTP2FrameContentType)
	_, _ = writer.Write(encoded)
}

func (server *Server) recordHTTP2Rejected() {
	server.mu.Lock()
	server.stats.RejectedRequests++
	server.mu.Unlock()
}

func isHTTP2FrameContentType(value string) bool {
	contentType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	return err == nil && contentType == transport.HTTP2FrameContentType
}
