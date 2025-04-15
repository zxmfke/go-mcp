package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ThinkInAIXYZ/go-mcp/pkg"
	"github.com/ThinkInAIXYZ/go-mcp/protocol"
	"github.com/ThinkInAIXYZ/go-mcp/server/session"
	"github.com/ThinkInAIXYZ/go-mcp/transport"
)

type Option func(*Server)

func WithCapabilities(capabilities protocol.ServerCapabilities) Option {
	return func(s *Server) {
		s.capabilities = &capabilities
	}
}

func WithServerInfo(serverInfo protocol.Implementation) Option {
	return func(s *Server) {
		s.serverInfo = &serverInfo
	}
}

func WithInstructions(instructions string) Option {
	return func(s *Server) {
		s.instructions = instructions
	}
}

func WithLogger(logger pkg.Logger) Option {
	return func(s *Server) {
		s.logger = logger
	}
}

type Server struct {
	transport transport.ServerTransport

	tools             pkg.SyncMap[*toolEntry]
	prompts           pkg.SyncMap[*promptEntry]
	resources         pkg.SyncMap[*resourceEntry]
	resourceTemplates pkg.SyncMap[*resourceTemplateEntry]

	sessionManager *session.Manager

	inShutdown   atomic.Value // true when server is in shutdown
	inFlyRequest sync.WaitGroup

	capabilities *protocol.ServerCapabilities
	serverInfo   *protocol.Implementation
	instructions string

	logger pkg.Logger
}

func NewServer(t transport.ServerTransport, opts ...Option) (*Server, error) {
	server := &Server{
		transport: t,
		capabilities: &protocol.ServerCapabilities{
			Prompts:   &protocol.PromptsCapability{ListChanged: true},
			Resources: &protocol.ResourcesCapability{ListChanged: true, Subscribe: true},
			Tools:     &protocol.ToolsCapability{ListChanged: true},
		},
		sessionManager: session.NewManager(),
		inShutdown:     *pkg.NewBoolAtomic(),
		serverInfo:     &protocol.Implementation{},
		logger:         pkg.DefaultLogger,
	}
	t.SetReceiver(transport.ServerReceiverF(server.receive))

	for _, opt := range opts {
		opt(server)
	}

	t.SetSessionManager(server.sessionManager)

	return server, nil
}

func (server *Server) Run() error {
	errCh := make(chan error, 1)
	go func() {
		defer pkg.Recover()

		errCh <- server.transport.Run()
	}()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("init mcp server transpor run fail: %w", err)
			}
			return nil
		case <-ticker.C:
			server.sessionManager.RangeSessions(func(key string, _ *session.State) bool {
				if server.inShutdown.Load().(bool) {
					return false
				}

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				if _, err := server.Ping(setSessionIDToCtx(ctx, key), protocol.NewPingRequest()); err != nil {
					server.logger.Warnf("sessionID=%s ping failed: %v", key, err)
					if errors.Is(err, pkg.ErrLackSession) {
						server.sessionManager.CloseSession(key)
					}
				}
				return true
			})
		}
	}
}

type toolEntry struct {
	tool    *protocol.Tool
	handler ToolHandlerFunc
}

type ToolHandlerFunc func(*protocol.CallToolRequest) (*protocol.CallToolResult, error)

func (server *Server) RegisterTool(tool *protocol.Tool, toolHandler ToolHandlerFunc) {
	server.tools.Store(tool.Name, &toolEntry{tool: tool, handler: toolHandler})
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ToolListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification toll list changes fail: %v", err)
			return
		}
	}
}

func (server *Server) UnregisterTool(name string) {
	server.tools.Delete(name)
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ToolListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification toll list changes fail: %v", err)
			return
		}
	}
}

type promptEntry struct {
	prompt  *protocol.Prompt
	handler PromptHandlerFunc
}

type PromptHandlerFunc func(*protocol.GetPromptRequest) (*protocol.GetPromptResult, error)

func (server *Server) RegisterPrompt(prompt *protocol.Prompt, promptHandler PromptHandlerFunc) {
	server.prompts.Store(prompt.Name, &promptEntry{prompt: prompt, handler: promptHandler})
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4PromptListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification prompt list changes fail: %v", err)
			return
		}
	}
}

func (server *Server) UnregisterPrompt(name string) {
	server.prompts.Delete(name)
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4PromptListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification prompt list changes fail: %v", err)
			return
		}
	}
}

type resourceEntry struct {
	resource *protocol.Resource
	handler  ResourceHandlerFunc
}

type ResourceHandlerFunc func(*protocol.ReadResourceRequest) (*protocol.ReadResourceResult, error)

func (server *Server) RegisterResource(resource *protocol.Resource, resourceHandler ResourceHandlerFunc) {
	server.resources.Store(resource.URI, &resourceEntry{resource: resource, handler: resourceHandler})
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ResourceListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification resource list changes fail: %v", err)
			return
		}
	}
}

func (server *Server) UnregisterResource(uri string) {
	server.resources.Delete(uri)
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ResourceListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification resource list changes fail: %v", err)
			return
		}
	}
}

type resourceTemplateEntry struct {
	resourceTemplate *protocol.ResourceTemplate
	handler          ResourceHandlerFunc
}

func (server *Server) RegisterResourceTemplate(resource *protocol.ResourceTemplate, resourceHandler ResourceHandlerFunc) error {
	if err := resource.ParseURITemplate(); err != nil {
		return err
	}
	server.resourceTemplates.Store(resource.URITemplate, &resourceTemplateEntry{resourceTemplate: resource, handler: resourceHandler})
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ResourceListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification resource list changes fail: %v", err)
			return nil
		}
	}
	return nil
}

func (server *Server) UnregisterResourceTemplate(uriTemplate string) {
	server.resourceTemplates.Delete(uriTemplate)
	if !server.sessionManager.IsEmpty() {
		if err := server.sendNotification4ResourceListChanges(context.Background()); err != nil {
			server.logger.Warnf("send notification resource list changes fail: %v", err)
			return
		}
	}
}

func (server *Server) Shutdown(userCtx context.Context) error {
	server.inShutdown.Store(true)

	serverCtx, cancel := context.WithCancel(userCtx)
	defer cancel()

	go func() {
		defer pkg.Recover()

		server.inFlyRequest.Wait()
		cancel()
	}()

	return server.transport.Shutdown(userCtx, serverCtx)
}
