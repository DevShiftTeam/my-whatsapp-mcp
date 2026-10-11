package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const adapterRequestTimeout = 5 * time.Second

var errNativeStdioCleanup = errors.New("mcp_stdio_cleanup_failed")

// Only a response from the pinned daemon can produce this private marker.
var errAdapterHTTPUnauthorized = errors.New("mcp_adapter_http_unauthorized")

// RunStdio exposes the daemon's four read-only tools over the SDK's native stdio
// transport. It never starts a listener, pairs an account or owns model work.
// The SDK protocol is the only output written to stdout. The caller grants
// exclusive stdin/stdout use while active; their descriptors and flags are
// preserved when it returns.
func RunStdio(ctx context.Context, config AdapterConfig) error {
	if !validNativeAdapterConfig(config) {
		return ErrConfiguration
	}
	transport, cleanup, err := nativeStdioTransport(ctx)
	if err != nil {
		return err
	}
	return runWithNativeStdioCleanup(func() error {
		return runStdioTransport(ctx, config, transport)
	}, cleanup)
}

func runWithNativeStdioCleanup(run func() error, cleanup func() error) (result error) {
	defer func() {
		if cleanup() != nil {
			result = errNativeStdioCleanup
		}
	}()
	return run()
}

func rollbackNativeStdio(closeOwned func() error, restoreFlags func() error) error {
	closeErr := closeOwned()
	restoreErr := restoreFlags()
	if closeErr != nil || restoreErr != nil {
		return errNativeStdioCleanup
	}
	return ErrConfiguration
}

func newNativeStdioClose(closers [2]func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			for _, closeOwned := range closers {
				if closeOwned != nil {
					if closeOwned() != nil {
						result = errNativeStdioCleanup
					}
				}
			}
		})
		return result
	}
}

func newNativeStdioCleanup(stop func() bool, closeOwned func() error, restoreFlags func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			stop()
			closeErr := closeOwned()
			restoreErr := restoreFlags()
			if closeErr != nil || restoreErr != nil {
				result = errNativeStdioCleanup
			}
		})
		return result
	}
}

// boundedAdapterBody reports overflow instead of pretending a truncated reply
// was complete. The underlying connection is always closed by the caller.
type boundedAdapterBody struct {
	source    io.ReadCloser
	remaining int64
}

func (b *boundedAdapterBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var extra [1]byte
		n, err := b.source.Read(extra[:])
		if n != 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.source.Read(p)
	b.remaining -= int64(n)
	return n, err
}
func (b *boundedAdapterBody) Close() error { return b.source.Close() }

type adapterHTTPTransport struct {
	endpoint, token string
	transport       *http.Transport
}

func (t *adapterHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.endpoint != NativeAdapterEndpoint || request.URL == nil || request.URL.String() != t.endpoint || (request.Host != "" && request.Host != "localhost") {
		return nil, ErrUnavailable
	}
	copy := request.Clone(request.Context())
	if copy.Header == nil {
		copy.Header = make(http.Header)
	}
	copy.Header.Del("Authorization")
	copy.Header.Del(accessHeader)
	copy.Header.Set(localAdapterHeader, t.token)
	response, err := t.transport.RoundTrip(copy)
	if err != nil {
		return nil, ErrUnavailable
	}
	if response.ContentLength > int64(defaultResponseBytes) {
		_ = response.Body.Close()
		return nil, ErrResponseTooLarge
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		return nil, errAdapterHTTPUnauthorized
	}
	response.Body = &boundedAdapterBody{source: response.Body, remaining: int64(defaultResponseBytes)}
	return response, nil
}

func newAdapterHTTPTransport(config AdapterConfig) (*http.Transport, error) {
	if !validNativeAdapterConfig(config) {
		return nil, ErrConfiguration
	}
	// Proxies and redirects are disabled so the token cannot leave the pinned
	// Unix socket. The logical HTTP authority never causes a TCP or DNS dial.
	// Each connection is independently validated, including the daemon peer UID,
	// before the dialer returns a connection that can carry a credential header.
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "localhost:80" {
				return nil, ErrUnavailable
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			conn, err := appdata.DialNativeSocket(ctx, config.NativeSocketPath)
			if err != nil || conn == nil {
				if conn != nil {
					_ = conn.Close()
				}
				return nil, ErrUnavailable
			}
			return conn, nil
		},
		MaxIdleConns: 4, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4,
		IdleConnTimeout: 10 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
		MaxResponseHeaderBytes: 8 << 10,
	}, nil
}

func runStdioTransport(ctx context.Context, config AdapterConfig, transport mcp.Transport) error {
	if !validNativeAdapterConfig(config) || transport == nil {
		return ErrConfiguration
	}
	httpTransport, err := newAdapterHTTPTransport(config)
	if err != nil {
		return err
	}
	defer httpTransport.CloseIdleConnections()
	httpClient := &http.Client{
		Transport:     &adapterHTTPTransport{endpoint: config.Endpoint, token: config.Token, transport: httpTransport},
		Timeout:       adapterRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := mcp.NewClient(&mcp.Implementation{Name: "wa-indexer-stdio-adapter", Version: "1.0.0"}, &mcp.ClientOptions{Logger: quiet})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: config.Endpoint, HTTPClient: httpClient,
		DisableStandaloneSSE: true, MaxRetries: -1, MaxEventSize: defaultResponseBytes,
	}, nil)
	if err != nil {
		return ErrUnavailable
	}
	defer session.Close()
	discoveryCtx, discoveryCancel := context.WithTimeout(ctx, adapterRequestTimeout)
	listed, err := session.ListTools(discoveryCtx, nil)
	discoveryCancel()
	if err != nil || len(listed.Tools) != 4 {
		return ErrUnavailable
	}
	allowed := map[string]bool{"search_messages": true, "get_context": true, "list_chats": true, "get_index_status": true}
	server := mcp.NewServer(&mcp.Implementation{Name: "wa-indexer-stdio-adapter", Version: "1.0.0"}, &mcp.ServerOptions{Logger: quiet})
	semaphore := make(chan struct{}, 4)
	for _, tool := range listed.Tools {
		if tool == nil || !allowed[tool.Name] || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			return ErrUnavailable
		}
		delete(allowed, tool.Name)
		name := tool.Name
		server.AddTool(tool, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if !validAdapterArguments(request.Params.Arguments) {
				return adapterToolError("mcp_invalid_arguments"), nil
			}
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			default:
				return adapterToolError("mcp_busy"), nil
			}
			callCtx, cancel := context.WithTimeout(callCtx, adapterRequestTimeout)
			defer cancel()
			reply, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: name, Arguments: request.Params.Arguments})
			if err != nil || reply == nil {
				if callCtx.Err() == nil && errors.Is(err, errAdapterHTTPUnauthorized) {
					return adapterToolError(ErrUnauthorized.Error()), nil
				}
				return adapterToolError(ErrUnavailable.Error()), nil
			}
			data, err := json.Marshal(reply)
			if err != nil || len(data) > defaultResponseBytes {
				return adapterToolError(ErrResponseTooLarge.Error()), nil
			}
			if callCtx.Err() != nil {
				return adapterToolError(ErrUnavailable.Error()), nil
			}
			return reply, nil
		})
	}
	if err := server.Run(ctx, transport); err != nil {
		return ErrUnavailable
	}
	return nil
}

func validAdapterArguments(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if int64(len(raw)) > defaultRequestBytes {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func adapterToolError(code string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: code}}}
}
