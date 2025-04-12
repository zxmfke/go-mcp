package transport

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSSE(t *testing.T) {
	var (
		err    error
		svr    ServerTransport
		client ClientTransport
	)

	// Get an available port
	port, err := getAvailablePort()
	if err != nil {
		t.Fatalf("Failed to get available port: %v", err)
	}

	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	clientURL := fmt.Sprintf("http://%s/sse", serverAddr)

	if svr, err = NewSSEServerTransport(serverAddr); err != nil {
		t.Fatalf("NewSSEServerTransport failed: %v", err)
	}

	if client, err = NewSSEClientTransport(clientURL); err != nil {
		t.Fatalf("NewSSEClientTransport failed: %v", err)
	}

	testTransport(t, client, svr)
}

func TestSSEHandler(t *testing.T) {
	var (
		messageURL = "/message"
		port       int

		err    error
		svr    ServerTransport
		client ClientTransport
	)

	// Get an available port
	port, err = getAvailablePort()
	if err != nil {
		t.Fatalf("Failed to get available port: %v", err)
	}

	serverAddr := fmt.Sprintf("http://127.0.0.1:%d", port)
	serverURL := fmt.Sprintf("%s/sse", serverAddr)

	svr, handler, err := NewSSEServerTransportAndHandler(fmt.Sprintf("%s%s", serverAddr, messageURL))
	if err != nil {
		t.Fatalf("NewSSEServerTransport failed: %v", err)
	}

	// Set up HTTP routes
	http.Handle("/sse", handler.HandleSSE())
	http.Handle(messageURL, handler.HandleMessage())

	errCh := make(chan error, 1)
	go func() {
		if err = http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()

	// Use select to handle potential errors
	select {
	case err = <-errCh:
		t.Fatalf("http.ListenAndServe() failed: %v", err)
	case <-time.After(time.Second):
		// Server started normally
	}

	if client, err = NewSSEClientTransport(serverURL); err != nil {
		t.Fatalf("NewSSEClientTransport failed: %v", err)
	}

	testTransport(t, client, svr)
}

// Test SSE client options functionality
func TestSSEClientOptions(t *testing.T) {
	customLogger := &testLogger{}
	customTimeout := 5 * time.Second
	customClient := &http.Client{Timeout: 10 * time.Second}

	client, err := NewSSEClientTransport("http://example.com/sse",
		WithSSEClientOptionReceiveTimeout(customTimeout),
		WithSSEClientOptionHTTPClient(customClient),
		WithSSEClientOptionLogger(customLogger),
	)
	assert.NoError(t, err)

	sseClient, ok := client.(*sseClientTransport)
	assert.True(t, ok)

	assert.Equal(t, customTimeout, sseClient.receiveTimeout)
	assert.Equal(t, customClient, sseClient.client)
	assert.Equal(t, customLogger, sseClient.logger)
}

// Test SSE server options functionality
func TestSSEServerOptions(t *testing.T) {
	customLogger := &testLogger{}
	customSSEPath := "/custom-sse"
	customMsgPath := "/custom-message"
	customURLPrefix := "http://test.example.com"

	server, err := NewSSEServerTransport("localhost:8080",
		WithSSEServerTransportOptionLogger(customLogger),
		WithSSEServerTransportOptionSSEPath(customSSEPath),
		WithSSEServerTransportOptionMessagePath(customMsgPath),
		WithSSEServerTransportOptionURLPrefix(customURLPrefix),
	)
	assert.NoError(t, err)

	sseServer, ok := server.(*sseServerTransport)
	assert.True(t, ok)

	assert.Equal(t, customLogger, sseServer.logger)
	assert.Equal(t, customSSEPath, sseServer.ssePath)
	assert.Equal(t, customMsgPath, sseServer.messagePath)
	assert.Equal(t, customURLPrefix, sseServer.urlPrefix)
}

// Test SSE server error handling
func TestSSEServerErrorHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error"))
	}))
	defer server.Close()

	client, err := NewSSEClientTransport(server.URL)
	assert.NoError(t, err)

	err = client.Start()
	assert.Error(t, err)
}

// Test SSE server message handling errors
func TestSSEServerMessageHandling(t *testing.T) {
	svr, handler, err := NewSSEServerTransportAndHandler("http://example.com/message", WithSSEServerTransportAndHandlerOptionLogger(&testLogger{}))
	assert.NoError(t, err)

	// Test unsupported HTTP method
	req := httptest.NewRequest(http.MethodGet, "/message?sessionID=test", nil)
	rr := httptest.NewRecorder()
	handler.HandleMessage().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// Test missing session ID
	req = httptest.NewRequest(http.MethodPost, "/message", nil)
	rr = httptest.NewRecorder()
	handler.HandleMessage().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Test invalid session ID
	req = httptest.NewRequest(http.MethodPost, "/message?sessionID=invalid", nil)
	rr = httptest.NewRecorder()
	handler.HandleMessage().ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Shutdown server
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	svrCtx, svrCancel := context.WithCancel(context.Background())
	svrCancel()
	err = svr.Shutdown(ctx, svrCtx)
	assert.NoError(t, err)
}

// Test completeMessagePath function
func TestCompleteMessagePath(t *testing.T) {
	tests := []struct {
		name          string
		urlPrefix     string
		messagePath   string
		expectedURL   string
		expectedError bool
	}{
		{
			name:        "Valid URL",
			urlPrefix:   "http://example.com",
			messagePath: "/message",
			expectedURL: "http://example.com/message",
		},
		{
			name:        "URL with trailing slash",
			urlPrefix:   "http://example.com/",
			messagePath: "message",
			expectedURL: "http://example.com/message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, err := completeMessagePath(tt.urlPrefix, tt.messagePath)
			if tt.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedURL, url)
			}
		})
	}
}

// Test handleSSEEvent processing various event types
func TestHandleSSEEvent(t *testing.T) {
	transport := &sseClientTransport{
		ctx:            context.Background(),
		endpointChan:   make(chan struct{}, 1),
		logger:         &testLogger{},
		receiveTimeout: time.Second,
	}

	// Set up a receiver
	receivedMsg := make(chan []byte, 1)
	transport.SetReceiver(ClientReceiverF(func(ctx context.Context, msg []byte) error {
		receivedMsg <- msg
		return nil
	}))

	// Test endpoint event
	transport.handleSSEEvent("endpoint", "http://example.com/message?sessionID=test")
	assert.NotNil(t, transport.messageEndpoint)
	assert.Equal(t, "http://example.com/message?sessionID=test", transport.messageEndpoint.String())

	// Test message event
	transport.handleSSEEvent("message", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test\"}")
	select {
	case msg := <-receivedMsg:
		assert.Equal(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test\"}", string(msg))
	case <-time.After(time.Second):
		t.Fatal("Message not received")
	}
}

// Test readSSE function
func TestReadSSE(t *testing.T) {
	transport := &sseClientTransport{
		ctx:            context.Background(),
		cancel:         func() {},
		endpointChan:   make(chan struct{}, 1),
		logger:         &testLogger{},
		receiveTimeout: time.Second,
	}

	// Create test SSE data
	sseData := `
event: endpoint
data: http://example.com/message

event: message
data: {"jsonrpc":"2.0","id":1,"method":"test"}

`
	// Set up a receiver
	receivedMsg := make(chan []byte, 1)
	transport.SetReceiver(ClientReceiverF(func(ctx context.Context, msg []byte) error {
		receivedMsg <- msg
		return nil
	}))

	// Create a reader
	reader := io.NopCloser(strings.NewReader(sseData))

	// Start reading
	go transport.readSSE(reader)

	// Wait for endpoint processing
	select {
	case <-transport.endpointChan:
		assert.NotNil(t, transport.messageEndpoint)
	case <-time.After(time.Second):
		t.Fatal("Endpoint not received")
	}

	// Wait for message processing
	select {
	case msg := <-receivedMsg:
		assert.Equal(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test\"}", string(msg))
	case <-time.After(time.Second):
		t.Fatal("Message not received")
	}
}

// Test SSE client start with unavailable url
func TestSSEClientStartError(t *testing.T) {
	client, _ := NewSSEClientTransport("http://127.0.0.1:6666/sse")
	err := client.Start()
	assert.Error(t, err)
}

// getAvailablePort returns a port that is available for use
func getAvailablePort() (int, error) {
	addr, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("failed to get available port: %v", err)
	}
	defer func() {
		if err = addr.Close(); err != nil {
			fmt.Println(err)
		}
	}()

	port := addr.Addr().(*net.TCPAddr).Port
	return port, nil
}
