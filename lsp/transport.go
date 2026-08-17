package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
	"go.lsp.dev/protocol"
)

const (
	DefaultMaxMessageBytes = int64(4 << 20)
	maxHeaderBytes         = 8 << 10
)

var (
	ErrMessageTooLarge    = errors.New("LSP message exceeds limit")
	ErrMalformedTransport = errors.New("malformed LSP transport")
)

// MessageHandler receives server-to-client calls. Its result is used only for
// requests; notification results are discarded.
type MessageHandler func(context.Context, string, json.RawMessage) (any, error)

// Transport is the narrow fakeable JSON-RPC seam used by the LSP lifecycle.
type Transport interface {
	Call(context.Context, string, any, any) error
	Notify(context.Context, string, any) error
	Done() <-chan struct{}
	Close() error
}

// RPCTransport is a bounded sourcegraph/jsonrpc2 connection.
type RPCTransport struct {
	conn          *jsonrpc2.Conn
	connection    *ownedConnection
	stopLifecycle func() bool
}

// NewRPCTransport constructs a bounded Content-Length framed connection.
// The context owns the connection lifetime.
func NewRPCTransport(ctx context.Context, connection io.ReadWriteCloser, maximum int64, handler MessageHandler) (*RPCTransport, error) {
	if ctx == nil || connection == nil {
		return nil, fmt.Errorf("new LSP transport: nil context or connection")
	}
	if maximum <= 0 {
		maximum = DefaultMaxMessageBytes
	}
	owned := &ownedConnection{ReadWriteCloser: connection}
	stream := jsonrpc2.NewBufferedStream(owned, boundedCodec{maximum: maximum})
	rpcHandler := &incomingHandler{handle: handler, lifecycle: ctx}
	conn := jsonrpc2.NewConn(context.Background(), stream, rpcHandler, jsonrpc2.SetLogger(log.New(io.Discard, "", 0)))
	transport := &RPCTransport{conn: conn, connection: owned}
	transport.stopLifecycle = context.AfterFunc(ctx, func() { _ = owned.Close() })
	return transport, nil
}

// Call sends one bounded LSP request and decodes its result with the protocol
// package's union-aware codec.
func (transport *RPCTransport) Call(ctx context.Context, method string, params, result any) error {
	if transport == nil || transport.conn == nil {
		return jsonrpc2.ErrClosed
	}
	if ctx == nil {
		return fmt.Errorf("LSP call: nil context")
	}
	encoded, err := marshalPayload(params)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = transport.runBounded(ctx, func() error { return transport.conn.Call(ctx, method, encoded, &raw) })
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	return protocol.Unmarshal(raw, result)
}

// Notify sends one bounded LSP notification.
func (transport *RPCTransport) Notify(ctx context.Context, method string, params any) error {
	if transport == nil || transport.conn == nil {
		return jsonrpc2.ErrClosed
	}
	if ctx == nil {
		return fmt.Errorf("LSP notify: nil context")
	}
	encoded, err := marshalPayload(params)
	if err != nil {
		return err
	}
	return transport.runBounded(ctx, func() error { return transport.conn.Notify(ctx, method, encoded) })
}

func (transport *RPCTransport) runBounded(ctx context.Context, operation func() error) error {
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = transport.connection.Close()
		close(interrupted)
	})
	finished := false
	finish := func() {
		if finished {
			return
		}
		finished = true
		if !stopInterrupt() {
			<-interrupted
		}
	}
	defer finish()
	err := operation()
	finish()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// Done closes when the underlying JSON-RPC stream disconnects.
func (transport *RPCTransport) Done() <-chan struct{} {
	if transport == nil || transport.conn == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return transport.conn.DisconnectNotify()
}

// Close closes the connection.
func (transport *RPCTransport) Close() error {
	if transport == nil || transport.connection == nil {
		return nil
	}
	if transport.stopLifecycle != nil {
		transport.stopLifecycle()
	}
	return transport.connection.Close()
}

func marshalPayload(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	if raw, ok := value.(json.RawMessage); ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	encoded, err := protocol.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

type incomingHandler struct {
	handle    MessageHandler
	lifecycle context.Context
}

func (handler *incomingHandler) Handle(_ context.Context, conn *jsonrpc2.Conn, request *jsonrpc2.Request) {
	ctx := handler.lifecycle
	defer func() {
		if recover() != nil && !request.Notif {
			_ = conn.ReplyWithError(ctx, request.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: "LSP handler panic"})
		}
	}()
	raw := json.RawMessage("null")
	if request.Params != nil {
		raw = append(raw[:0], (*request.Params)...)
	}
	var (
		result any
		err    error
	)
	if handler != nil && handler.handle != nil {
		result, err = handler.handle(ctx, request.Method, raw)
	}
	if request.Notif {
		return
	}
	if err != nil {
		_ = conn.ReplyWithError(ctx, request.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: err.Error()})
		return
	}
	_ = conn.Reply(ctx, request.ID, result)
}

type boundedCodec struct {
	maximum int64
}

func (codec boundedCodec) WriteObject(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)) > codec.maximum {
		return ErrMessageTooLarge
	}
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func (codec boundedCodec) ReadObject(reader *bufio.Reader, value any) error {
	var (
		contentLength int64 = -1
		headerBytes   int
	)
	for {
		line, err := reader.ReadSlice('\n')
		headerBytes += len(line)
		if headerBytes > maxHeaderBytes || errors.Is(err, bufio.ErrBufferFull) {
			return ErrMessageTooLarge
		}
		if err != nil {
			return err
		}
		if string(line) == "\r\n" {
			break
		}
		if len(line) < 2 || line[len(line)-2] != '\r' {
			return ErrMalformedTransport
		}
		name, rawValue, found := strings.Cut(string(line[:len(line)-2]), ":")
		if !found {
			return ErrMalformedTransport
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			if contentLength >= 0 {
				return ErrMalformedTransport
			}
			rawValue = strings.TrimSpace(rawValue)
			parsed, parseErr := strconv.ParseInt(rawValue, 10, 64)
			if parseErr != nil || !decimalDigits(rawValue) {
				return ErrMalformedTransport
			}
			contentLength = parsed
		}
	}
	if contentLength < 0 {
		return ErrMalformedTransport
	}
	if contentLength > codec.maximum {
		return ErrMessageTooLarge
	}
	if uint64(contentLength) > uint64(^uint(0)>>1) {
		return ErrMessageTooLarge
	}
	data := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedTransport, err)
	}
	return nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range []byte(value) {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

type stdioConnection struct {
	reader io.ReadCloser
	writer io.WriteCloser
	once   sync.Once
	err    error
}

type ownedConnection struct {
	io.ReadWriteCloser
	once sync.Once
	err  error
}

func (connection *ownedConnection) Close() error {
	connection.once.Do(func() { connection.err = connection.ReadWriteCloser.Close() })
	return connection.err
}

func (connection *stdioConnection) Read(data []byte) (int, error) {
	return connection.reader.Read(data)
}

func (connection *stdioConnection) Write(data []byte) (int, error) {
	return connection.writer.Write(data)
}

func (connection *stdioConnection) Close() error {
	connection.once.Do(func() {
		connection.err = errors.Join(connection.writer.Close(), connection.reader.Close())
	})
	return connection.err
}
