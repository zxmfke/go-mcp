package transport

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThinkInAIXYZ/go-mcp/pkg"

	"github.com/stretchr/testify/assert"
)

type mock struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	closer io.Closer
}

func (m *mock) Write(p []byte) (n int, err error) {
	return m.writer.Write(p)
}

func (m *mock) Close() error {
	if err := m.writer.Close(); err != nil {
		return err
	}
	if err := m.reader.Close(); err != nil {
		return err
	}
	if err := m.closer.Close(); err != nil {
		return err
	}
	return nil
}

func TestStdioTransport(t *testing.T) {
	var (
		err    error
		server *stdioServerTransport
		client *stdioClientTransport
	)

	mockServerTrPath := filepath.Join(os.TempDir(), "mock_server_tr_"+strconv.Itoa(rand.Int()))
	if err = compileMockStdioServerTr(mockServerTrPath); err != nil {
		t.Fatalf("Failed to compile mock server: %v", err)
	}

	defer func(name string) {
		if err = os.Remove(name); err != nil {
			fmt.Printf("Failed to remove mock server: %v\n", err)
		}
	}(mockServerTrPath)

	clientT, err := NewStdioClientTransport(mockServerTrPath, []string{})
	if err != nil {
		t.Fatalf("NewStdioClientTransport failed: %v", err)
	}

	client = clientT.(*stdioClientTransport)
	server = NewStdioServerTransport().(*stdioServerTransport)

	// Create pipes for communication
	reader1, writer1 := io.Pipe()
	reader2, writer2 := io.Pipe()

	// Set up the communication channels
	server.reader = reader2
	server.writer = writer1
	client.reader = reader1
	client.writer = &mock{
		reader: reader1,
		writer: writer2,
		closer: client.writer,
	}

	testTransport(t, client, server)
}

// Test StdioServerTransport options
func TestStdioServerOptions(t *testing.T) {
	customLogger := newTestLogger()

	server := NewStdioServerTransport(
		WithStdioServerOptionLogger(customLogger),
	).(*stdioServerTransport)

	assert.Equal(t, customLogger, server.logger)
}

// Test StdioClientTransport options
func TestStdioClientOptions(t *testing.T) {
	customLogger := newTestLogger()
	customEnv := []string{"DREAM=WORLDPEACE"}

	client, err := NewStdioClientTransport("echo", []string{},
		WithStdioClientOptionLogger(customLogger),
		WithStdioClientOptionEnv(customEnv...),
	)
	assert.NoError(t, err)

	stdioClient := client.(*stdioClientTransport)
	assert.Equal(t, customLogger, stdioClient.logger)

	// check env
	found := false
	for _, env := range stdioClient.cmd.Env {
		if env == customEnv[0] {
			found = true
			break
		}
	}
	assert.True(t, found)
}

func newStdioServerWithReader(reader io.ReadCloser) *stdioServerTransport {
	server := NewStdioServerTransport().(*stdioServerTransport)
	server.reader = reader
	return server
}

// Test StdioServerTransport Shutdown functionality
func TestStdioServerShutdown(t *testing.T) {
	reader, writer := io.Pipe()
	server := newStdioServerWithReader(reader)

	// Start a goroutine to run the server
	go func() {
		err := server.Run()
		assert.NoError(t, err)
	}()

	// Wait for the server to start
	time.Sleep(100 * time.Millisecond)

	// Normal shutdown
	userCtx, userCancel := context.WithTimeout(context.Background(), time.Second)
	defer userCancel()

	serverCtx, serverCancel := context.WithCancel(context.Background())

	// Trigger server shutdown
	_ = writer.Close()
	serverCancel()

	err := server.Shutdown(userCtx, serverCtx)
	assert.NoError(t, err)
}

// Test StdioServerTransport Shutdown when user ctx cancel
func TestStdioServerCancelByUserCtx(t *testing.T) {
	reader, writer := io.Pipe()
	server := newStdioServerWithReader(reader)

	go func() {
		err := server.Run()
		assert.NoError(t, err)
	}()

	// Wait for the server to start
	time.Sleep(100 * time.Millisecond)

	// Create a canceled context
	userCtx, userCancel := context.WithCancel(context.Background())
	userCancel()

	serverCtx, serverCancel := context.WithCancel(context.Background())

	// Don't trigger serverCtx cancellation, let it wait for user ctx timeout
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = writer.Close()
	}()

	err := server.Shutdown(userCtx, serverCtx)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)

	// Cleanup
	serverCancel()
}

// Test triggering receive shutdown done channel in StdioServerTransport
func TestStdioServerReceiveShutdownDone(t *testing.T) {
	reader, writer := io.Pipe()
	server := newStdioServerWithReader(reader)

	// Create a channel to signal when Run has returned
	runCompletedCh := make(chan struct{})

	go func() {
		err := server.Run()
		assert.NoError(t, err)
		close(runCompletedCh)
	}()

	// Wait for the server to start
	time.Sleep(100 * time.Millisecond)

	// Close the writer end of the pipe - this should cause the reader to get EOF
	// and the receive function to complete
	_ = writer.Close()

	// Wait for Run to complete, which should close receiveShutDone
	select {
	case <-runCompletedCh:
		// Run has completed, so receiveShutDone should be closed
	case <-time.After(time.Second):
		t.Fatal("Run did not complete within expected time")
	}

	userCtx, userCancel := context.WithTimeout(context.Background(), time.Second)
	defer userCancel()

	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	// Call Shutdown - this should select on the first case (receiveShutDone)
	err := server.Shutdown(userCtx, serverCtx)
	assert.NoError(t, err)

	// Verify that it's the receiveShutDone case that was triggered by ensuring
	// that the serverCtx is still not canceled
	select {
	case <-serverCtx.Done():
		t.Fatal("serverCtx was canceled, but should not have been")
	default:
		// serverCtx is still active, which means we exited from receiveShutDone
	}
}

// Test StdioServerTransport when the receiver returns an error
func TestStdioServerReceiveError(t *testing.T) {
	reader, writer := io.Pipe()
	server := newStdioServerWithReader(reader)

	// Set a receiver that will report an error when receiving data
	receiverCalled := make(chan struct{})
	server.SetReceiver(ServerReceiverF(func(_ context.Context, _ string, _ []byte) error {
		close(receiverCalled)
		return fmt.Errorf("server receiver error")
	}))

	// Start the server in a goroutine
	done := make(chan struct{})
	go func() {
		err := server.Run()
		assert.NoError(t, err)
		close(done)
	}()

	// Wait for the server to start
	time.Sleep(100 * time.Millisecond)

	// Write a message to the pipe, which will trigger the receive function
	// The message should be sent to the receiver, which will return an error
	_, err := writer.Write([]byte("test message\n"))
	assert.NoError(t, err)

	// Wait for the receiver to be called
	select {
	case <-receiverCalled:
		// Receiver was called, which is what we expect
	case <-time.After(time.Second):
		t.Fatal("Receiver was not called within expected time")
	}

	// The server should continue running despite the receiver error
	select {
	case <-done:
		t.Fatal("Server stopped unexpectedly")
	case <-time.After(100 * time.Millisecond):
		// Server is still running, which is the expected behavior
	}

	// Clean up the server
	userCtx, userCancel := context.WithTimeout(context.Background(), time.Second)
	defer userCancel()

	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	// Close the writer to cause the reader to get EOF
	_ = writer.Close()

	// Shutdown should complete without errors
	err = server.Shutdown(userCtx, serverCtx)
	assert.NoError(t, err)
}

// Test StdioServerTransport receive not ErrClosedPipe but else
func TestStdioServerReceiveNonErrClosedPipe(t *testing.T) {
	server := newStdioServerWithReader(&errorReader{})

	server.SetReceiver(ServerReceiverF(serverReceiveEmpty))

	// start server
	go func() {
		err := server.Run()
		assert.NoError(t, err)
	}()

	time.Sleep(100 * time.Millisecond)

	// 关闭服务器
	userCtx, userCancel := context.WithTimeout(context.Background(), time.Second)
	defer userCancel()

	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	err := server.Shutdown(userCtx, serverCtx)
	assert.NoError(t, err)
}

// Test StdioClientTransport error handling when command execution fails
func TestStdioClientStartFailure(t *testing.T) {
	client, err := NewStdioClientTransport("non_existent_command", []string{})
	assert.NoError(t, err) // Creation should succeed, but starting will fail

	err = client.Start()
	assert.Error(t, err)
}

// Test StdioClientTransport.Close error handling
func TestStdioClientCloseError(t *testing.T) {
	// Create a mock command that will start normally but fail to close properly
	client := &stdioClientTransport{
		cmd:             exec.Command("echo", "test"),
		writer:          &errorWriter{},
		logger:          pkg.DefaultLogger,
		receiveShutDone: make(chan struct{}),
	}

	// Set an empty receiver
	client.SetReceiver(ClientReceiverF(clientReceiveEmpty))

	// Simulate the receive function ending by closing receiveShutDone
	close(client.receiveShutDone)

	// Set a cancel function
	client.cancel = func() {}

	// Close should report an error
	err := client.Close()
	assert.Error(t, err)
}

// Test receive function error handling
func TestStdioClientReceiveError(_ *testing.T) {
	client := &stdioClientTransport{
		logger:          newTestLogger(),
		receiveShutDone: make(chan struct{}),
	}

	// Create a reader that will provide an error
	errorPipe := &errorReader{}
	client.reader = errorPipe

	// Set a receiver that will report an error
	client.SetReceiver(ClientReceiverF(clientReceiveError))

	// Create a context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the receive function
	go func() {
		client.receive(ctx)
		close(client.receiveShutDone)
	}()

	// Wait for processing to complete
	<-client.receiveShutDone
}

func compileMockStdioServerTr(outputPath string) error {
	cmd := exec.Command("go", "build", "-o", outputPath, "../testdata/mock_block_server.go")

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compilation failed: %v\nOutput: %s", err, output)
	}

	return nil
}
