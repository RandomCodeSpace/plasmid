package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBoundedCodec(t *testing.T) {
	codec := boundedCodec{maximum: 16}
	if err := codec.WriteObject(&bytes.Buffer{}, strings.Repeat("x", 32)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized write error = %v", err)
	}
	reader := bufio.NewReader(strings.NewReader("Content-Length: 17\r\n\r\n" + strings.Repeat("x", 17)))
	if err := codec.ReadObject(reader, new(any)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized read error = %v", err)
	}
	reader = bufio.NewReader(strings.NewReader("Content-Type: application/json\r\n\r\n{}"))
	if err := codec.ReadObject(reader, new(any)); !errors.Is(err, ErrMalformedTransport) {
		t.Fatalf("missing length error = %v", err)
	}
	reader = bufio.NewReader(strings.NewReader("Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}"))
	if err := codec.ReadObject(reader, new(any)); !errors.Is(err, ErrMalformedTransport) {
		t.Fatalf("duplicate length error = %v", err)
	}
	reader = bufio.NewReader(strings.NewReader("Content-Length: +2\r\n\r\n{}"))
	if err := codec.ReadObject(reader, new(any)); !errors.Is(err, ErrMalformedTransport) {
		t.Fatalf("signed length error = %v", err)
	}
}

func TestRPCTransportScriptedRoundTrip(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notified := make(chan json.RawMessage, 1)
	server, err := NewRPCTransport(ctx, serverConnection, 1024, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "notice" {
			notified <- append(json.RawMessage(nil), raw...)
			return nil, nil
		}
		return map[string]string{"answer": "ok"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := NewRPCTransport(ctx, clientConnection, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	var result map[string]string
	if err := client.Call(ctx, "question", map[string]string{"ask": "now"}, &result); err != nil {
		t.Fatal(err)
	}
	if result["answer"] != "ok" {
		t.Fatalf("result = %#v", result)
	}
	if err := client.Notify(ctx, "notice", map[string]int{"value": 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-notified:
		if string(raw) != `{"value":1}` {
			t.Fatalf("notification = %s", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not received")
	}
}

func TestRPCTransportCallTimeout(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := NewRPCTransport(ctx, serverConnection, 1024, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := NewRPCTransport(ctx, clientConnection, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	requestContext, requestCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer requestCancel()
	if err := client.Call(requestContext, "blocked", nil, new(any)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v", err)
	}
}

func TestRPCTransportTimeoutInterruptsBlockedWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		send func(context.Context, *RPCTransport) error
	}{
		{name: "call", send: func(ctx context.Context, transport *RPCTransport) error {
			return transport.Call(ctx, "blocked", map[string]string{"value": "server never reads"}, new(any))
		}},
		{name: "notify", send: func(ctx context.Context, transport *RPCTransport) error {
			return transport.Notify(ctx, "blocked", map[string]string{"value": "server never reads"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientConnection, serverConnection := net.Pipe()
			defer func() { _ = serverConnection.Close() }()
			transport, err := NewRPCTransport(context.Background(), clientConnection, 1024, nil)
			if err != nil {
				t.Fatal(err)
			}
			requestContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			err = test.send(requestContext, transport)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("send error = %v", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("blocked write exceeded deadline bound: %v", elapsed)
			}
			select {
			case <-transport.Done():
			case <-time.After(time.Second):
				t.Fatal("timed-out transport remained open")
			}
		})
	}
}

func TestRPCTransportCancellationRacesLateResponse(t *testing.T) {
	for iteration := range 200 {
		clientConnection, serverConnection := net.Pipe()
		transport, err := NewRPCTransport(context.Background(), clientConnection, 1024, nil)
		if err != nil {
			t.Fatal(err)
		}
		requestRead := make(chan json.RawMessage, 1)
		releaseResponse := make(chan struct{})
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			defer func() { _ = serverConnection.Close() }()
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if err := (boundedCodec{maximum: 1024}).ReadObject(bufio.NewReader(serverConnection), &request); err != nil {
				return
			}
			requestRead <- request.ID
			<-releaseResponse
			_ = (boundedCodec{maximum: 1024}).WriteObject(serverConnection, map[string]any{
				"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"ok": true},
			})
		}()
		ctx, cancel := context.WithCancel(context.Background())
		callDone := make(chan error, 1)
		go func() {
			var result map[string]bool
			callDone <- transport.Call(ctx, "race", nil, &result)
		}()
		select {
		case <-requestRead:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d request was not read", iteration)
		}
		if iteration%2 == 0 {
			cancel()
			close(releaseResponse)
		} else {
			close(releaseResponse)
			cancel()
		}
		select {
		case <-callDone:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d call did not finish", iteration)
		}
		_ = transport.Close()
		<-serverDone
	}
}

func TestRPCTransportContainsHandlerPanic(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := NewRPCTransport(ctx, serverConnection, 1024, func(context.Context, string, json.RawMessage) (any, error) {
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	client, err := NewRPCTransport(ctx, clientConnection, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Call(ctx, "panic", nil, new(any)); err == nil {
		t.Fatal("handler panic reported success")
	}
}
