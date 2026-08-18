package lsp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const processWaitBound = 3 * time.Second

func startStdioProcess(ctx context.Context, executable string, arguments []string, root string, maximum int64, handler MessageHandler) (Transport, error) {
	if ctx == nil {
		return nil, errors.New("start LSP process: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	processContext, cancel := context.WithCancel(ctx)
	command := exec.Command(executable, arguments...)
	command.Dir = root
	command.Stderr = io.Discard
	command.WaitDelay = processWaitBound
	if err := configureProcessTree(command); err != nil {
		cancel()
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, err
	}
	tree, err := attachProcessTree(command.Process)
	if err != nil {
		cancel()
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	connection := &stdioConnection{reader: stdout, writer: stdin}
	rpc, err := NewRPCTransport(processContext, connection, maximum, handler)
	if err != nil {
		cancel()
		_ = tree.terminate()
		_ = command.Wait()
		return nil, err
	}
	transport := &processTransport{
		rpc:    rpc,
		cancel: cancel,
		done:   make(chan struct{}),
		tree:   tree,
	}
	go func() {
		transport.waitErr = command.Wait()
		_ = connection.Close()
		transport.stop()
		close(transport.done)
	}()
	go func() {
		select {
		case <-processContext.Done():
			transport.stop()
		case <-transport.done:
		}
	}()
	go transport.watchRPCDisconnect()
	return transport, nil
}

type processTransport struct {
	rpc     *RPCTransport
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	waitErr error
	stopErr error
	tree    processTree
}

func (transport *processTransport) Call(ctx context.Context, method string, params, result any) error {
	return transport.rpc.Call(ctx, method, params, result)
}

func (transport *processTransport) Notify(ctx context.Context, method string, params any) error {
	return transport.rpc.Notify(ctx, method, params)
}

func (transport *processTransport) Done() <-chan struct{} { return transport.done }

func (transport *processTransport) Close() error {
	transport.stop()
	select {
	case <-transport.done:
		if transport.stopErr != nil {
			return transport.stopErr
		}
		if errors.Is(transport.waitErr, os.ErrProcessDone) || isExpectedExit(transport.waitErr) {
			return nil
		}
		return transport.waitErr
	case <-time.After(processWaitBound):
		return context.DeadlineExceeded
	}
}

func (transport *processTransport) stop() {
	transport.once.Do(func() {
		transport.cancel()
		transport.stopErr = errors.Join(transport.rpc.Close(), transport.tree.terminate())
	})
}

func (transport *processTransport) watchRPCDisconnect() {
	select {
	case <-transport.rpc.Done():
		transport.stop()
	case <-transport.done:
	}
}

func isExpectedExit(err error) bool {
	var exitError *exec.ExitError
	return err == nil || errors.As(err, &exitError)
}
