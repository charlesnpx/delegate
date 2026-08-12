package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
)

func TestTimeoutCapturingClientReconnectsAfterReadFailure(t *testing.T) {
	var connections struct {
		sync.Mutex
		count int
	}
	serveErr := make(chan error, 4)
	serveDone := make(chan struct{}, 2)
	oldConnect := connectPinnedAgentbus
	oldDial := dialAgentbusWire
	connectPinnedAgentbus = func(context.Context, client.Options) error { return nil }
	dialAgentbusWire = func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		connections.Lock()
		connections.count++
		connection := connections.count
		connections.Unlock()
		go serveReconnectTestConnection(serverConn, connection, serveErr, serveDone)
		return clientConn, nil
	}
	t.Cleanup(func() {
		connectPinnedAgentbus = oldConnect
		dialAgentbusWire = oldDial
	})

	c, err := newTimeoutCapturingClient(context.Background(), client.Options{
		SocketPath:       "ignored-in-test",
		Token:            "test-token",
		DisableAutoStart: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.JobStatus(context.Background(), client.JobStatusParams{JobID: "job_reconnected"}); err == nil {
		t.Fatal("first status request succeeded, want connection read failure")
	}
	status, err := c.JobStatus(context.Background(), client.JobStatusParams{JobID: "job_reconnected"})
	if err != nil {
		t.Fatalf("status after reconnect: %v", err)
	}
	if len(status.Jobs) != 1 || status.Jobs[0].JobID != "job_reconnected" {
		t.Fatalf("status after reconnect = %#v", status)
	}
	for range 2 {
		<-serveDone
	}
	connections.Lock()
	connectionCount := connections.count
	connections.Unlock()
	if connectionCount != 2 {
		t.Fatalf("connections = %d, want 2 after EOF", connectionCount)
	}

	select {
	case err := <-serveErr:
		t.Fatal(err)
	default:
	}
}

func serveReconnectTestConnection(conn net.Conn, connection int, errCh chan<- error, done chan<- struct{}) {
	defer conn.Close()
	defer func() { done <- struct{}{} }()
	reader := bufio.NewReader(conn)
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		if connection != 1 && connection != 3 {
			errCh <- err
		}
		return
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"protocolVersion": 2,
		},
	}); err != nil {
		errCh <- err
		return
	}
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		errCh <- err
		return
	}
	if connection == 1 {
		return
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result": map[string]any{
			"jobs": []client.JobStatus{{
				JobID: "job_reconnected",
				State: engine.StateRunning,
			}},
		},
	}); err != nil {
		errCh <- err
	}
}
