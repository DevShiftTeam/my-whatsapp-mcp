//go:build !unix

package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func currentNativeStdioFlags() ([2]int, error) { return [2]int{}, ErrConfiguration }

// Unsupported platforms fail closed until their pipe ownership/deadline
// handling has equivalent native verification.
func nativeStdioTransport(context.Context) (mcp.Transport, func() error, error) {
	return nil, nil, ErrConfiguration
}
