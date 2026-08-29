package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RandomCodeSpace/plasmid/internal/processtree"
)

type commandTransport struct {
	command *exec.Cmd
	maximum int64
}

func newCommandTransport(command *exec.Cmd, maximum int64) (*commandTransport, error) {
	if err := processtree.Configure(command); err != nil {
		return nil, err
	}
	return &commandTransport{command: command, maximum: maximum}, nil
}

func (transport *commandTransport) Connect(context.Context) (sdkmcp.Connection, error) {
	stdout, err := transport.command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := transport.command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := transport.command.Start(); err != nil {
		return nil, err
	}
	tree, err := processtree.Attach(transport.command.Process)
	if err != nil {
		_ = transport.command.Process.Kill()
		_ = transport.command.Wait()
		return nil, err
	}
	connection := newProcessConnection(transport.command, stdin, stdout, tree, transport.maximum)
	return connection, nil
}

type processConnection struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	tree    processtree.Terminator
	maximum int64

	incoming  chan processMessage
	done      chan struct{}
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

type processMessage struct {
	message jsonrpc.Message
	err     error
}

func newProcessConnection(command *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser, tree processtree.Terminator, maximum int64) *processConnection {
	connection := &processConnection{
		command: command, stdin: stdin, stdout: stdout, tree: tree, maximum: maximum,
		incoming: make(chan processMessage), done: make(chan struct{}),
	}
	go connection.readLoop()
	return connection
}

func (connection *processConnection) SessionID() string { return "" }

func (connection *processConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-connection.incoming:
		return value.message, value.err
	case <-connection.done:
		return nil, io.EOF
	}
}

func (connection *processConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil {
		return err
	}
	if int64(len(encoded)+1) > connection.maximum {
		return fmt.Errorf("MCP stdio frame exceeds %d bytes", connection.maximum)
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-connection.done:
		return io.ErrClosedPipe
	default:
	}
	encoded = append(encoded, '\n')
	_, err = connection.stdin.Write(encoded)
	return err
}

func (connection *processConnection) Close() error {
	connection.closeOnce.Do(func() {
		close(connection.done)
		_ = connection.stdin.Close()
		waited := make(chan struct{})
		go func() {
			_ = connection.command.Wait()
			close(waited)
		}()
		graceful := time.NewTimer(250 * time.Millisecond)
		select {
		case <-waited:
			if !graceful.Stop() {
				<-graceful.C
			}
		case <-graceful.C:
		}
		connection.closeErr = connection.tree.Terminate()
		_ = connection.stdout.Close()
		select {
		case <-waited:
		case <-time.After(defaultCloseGrace):
			connection.closeErr = errors.Join(connection.closeErr, errors.New("MCP stdio process did not exit within close grace"))
		}
	})
	return connection.closeErr
}

func (connection *processConnection) readLoop() {
	bufferSize := int(connection.maximum)
	reader := bufio.NewReaderSize(connection.stdout, bufferSize+1)
	for {
		frame, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || int64(len(frame)) > connection.maximum {
			connection.deliver(nil, fmt.Errorf("MCP stdio frame exceeds %d bytes", connection.maximum))
			return
		}
		frame = bytes.TrimSuffix(frame, []byte{'\n'})
		frame = bytes.TrimSuffix(frame, []byte{'\r'})
		if len(frame) > 0 {
			message, decodeErr := jsonrpc.DecodeMessage(frame)
			if decodeErr != nil {
				connection.deliver(nil, decodeErr)
				return
			}
			if !connection.deliver(message, nil) {
				return
			}
		}
		if err != nil {
			connection.deliver(nil, err)
			return
		}
	}
}

func (connection *processConnection) deliver(message jsonrpc.Message, err error) bool {
	select {
	case connection.incoming <- processMessage{message: message, err: err}:
		return true
	case <-connection.done:
		return false
	}
}
