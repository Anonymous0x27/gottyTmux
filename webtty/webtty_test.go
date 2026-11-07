package webtty

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"sync"
	"testing"
)

type pipePair struct {
	*io.PipeReader
	*io.PipeWriter
}

type mockSlave struct {
	*io.PipeReader
	*io.PipeWriter
	mu   sync.Mutex
	cols int
	rows int
}

func newMockSlave() (*mockSlave, *io.PipeReader, *io.PipeWriter) {
	slaveOutReader, slaveOutWriter := io.Pipe() // slave -> webtty (Read)
	slaveInReader, slaveInWriter := io.Pipe()   // webtty -> slave (Write)
	return &mockSlave{
		PipeReader: slaveOutReader,
		PipeWriter: slaveInWriter,
	}, slaveInReader, slaveOutWriter
}

func (s *mockSlave) WindowTitleVariables() map[string]interface{} {
	return map[string]interface{}{}
}

func (s *mockSlave) ResizeTerminal(columns int, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cols = columns
	s.rows = rows
	return nil
}

func TestWriteFromPTY(t *testing.T) {
	connInPipeReader, connInPipeWriter := io.Pipe() // webtty -> masterConn
	connOutPipeReader, connOutPipeWriter := io.Pipe()
	defer connOutPipeWriter.Close()

	conn := pipePair{
		connOutPipeReader,
		connInPipeWriter,
	}
	slave, _, slaveOutWriter := newMockSlave()
	defer slaveOutWriter.Close()

	dt, err := New(conn, slave, WithWindowTitle([]byte("test")))
	if err != nil {
		t.Fatalf("Unexpected error from New(): %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- dt.Run(ctx)
	}()

	// Drain initial SetWindowTitle message
	buf := make([]byte, 1024)
	n, err := connInPipeReader.Read(buf)
	if err != nil {
		t.Fatalf("Unexpected error reading init title: %s", err)
	}
	if buf[0] != SetWindowTitle {
		t.Fatalf("Expected SetWindowTitle (%c), got %c", SetWindowTitle, buf[0])
	}

	message := []byte("foobar")
	n, err = slaveOutWriter.Write(message)
	if err != nil {
		t.Fatalf("Unexpected error from Write(): %s", err)
	}
	if n != len(message) {
		t.Fatalf("Write() accepted `%d` for message `%s`", n, message)
	}

	n, err = connInPipeReader.Read(buf)
	if err != nil {
		t.Fatalf("Unexpected error from Read(): %s", err)
	}
	if buf[0] != Output {
		t.Fatalf("Unexpected message type `%c`", buf[0])
	}
	decoded := make([]byte, 1024)
	n, err = base64.StdEncoding.Decode(decoded, buf[1:n])
	if err != nil {
		t.Fatalf("Unexpected error from Decode(): %s", err)
	}
	if !bytes.Equal(decoded[:n], message) {
		t.Fatalf("Unexpected message received: `%s`", decoded[:n])
	}

	cancel()
	<-done
}

func TestWriteFromConn(t *testing.T) {
	connInPipeReader, connInPipeWriter := io.Pipe()   // webtty -> masterConn
	connOutPipeReader, connOutPipeWriter := io.Pipe() // masterConn -> webtty
	defer connOutPipeWriter.Close()

	conn := pipePair{
		connOutPipeReader,
		connInPipeWriter,
	}
	slave, slaveInReader, slaveOutWriter := newMockSlave()
	defer slaveOutWriter.Close()

	dt, err := New(conn, slave, WithPermitWrite(), WithWindowTitle([]byte("test")))
	if err != nil {
		t.Fatalf("Unexpected error from New(): %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- dt.Run(ctx)
	}()

	readBuf := make([]byte, 1024)
	// Drain initial SetWindowTitle message
	if _, err := connInPipeReader.Read(readBuf); err != nil {
		t.Fatalf("Unexpected error reading init title: %s", err)
	}

	// input ('1' + payload)
	message := []byte{Input, 'h', 'e', 'l', 'l', 'o', '\n'}
	n, err := connOutPipeWriter.Write(message)
	if err != nil {
		t.Fatalf("Unexpected error from Write(): %s", err)
	}
	if n != len(message) {
		t.Fatalf("Write() accepted `%d` for message `%s`", n, message)
	}

	n, err = slaveInReader.Read(readBuf)
	if err != nil {
		t.Fatalf("Unexpected error from slave Read(): %s", err)
	}
	if !bytes.Equal(readBuf[:n], message[1:]) {
		t.Fatalf("Unexpected message received: `%s`", readBuf[:n])
	}

	// ping ('2') -> pong ('2')
	pingMsg := []byte{Ping}
	if _, err := connOutPipeWriter.Write(pingMsg); err != nil {
		t.Fatalf("Unexpected error writing Ping: %s", err)
	}

	n, err = connInPipeReader.Read(readBuf)
	if err != nil {
		t.Fatalf("Unexpected error from Read(): %s", err)
	}
	if !bytes.Equal(readBuf[:n], []byte{Pong}) {
		t.Fatalf("Unexpected Pong message received: `%s`", readBuf[:n])
	}

	// resize ('3' + JSON) followed by ping/pong to synchronize before cancel
	resizeMsg := append([]byte{ResizeTerminal}, []byte(`{"Columns":132,"Rows":43}`)...)
	if _, err := connOutPipeWriter.Write(resizeMsg); err != nil {
		t.Fatalf("Unexpected error writing ResizeTerminal: %s", err)
	}
	if _, err := connOutPipeWriter.Write(pingMsg); err != nil {
		t.Fatalf("Unexpected error writing sync Ping: %s", err)
	}
	if _, err := connInPipeReader.Read(readBuf); err != nil {
		t.Fatalf("Unexpected error reading sync Pong: %s", err)
	}

	cancel()
	<-done

	slave.mu.Lock()
	cols, rows := slave.cols, slave.rows
	slave.mu.Unlock()
	if cols != 132 || rows != 43 {
		t.Fatalf("Expected resize 132x43, got %dx%d", cols, rows)
	}
}
