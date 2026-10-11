package main

import (
	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"net/url"
)

func remoteLaunchConfig(opts options) (*app.MCPConfig, error) {
	canonical, err := clientsetup.NormalizeRemoteURL(opts.mcpPublicURL)
	if err != nil || canonical != opts.mcpPublicURL {
		return nil, errArguments
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return nil, errArguments
	}
	return &app.MCPConfig{Remote: &app.RemoteMCPConfig{Listen: opts.mcpListen, PublicURL: canonical, HeaderAuthAvailable: opts.mcpHeaderAuth, BearerTokenEnv: "WA_INDEXER_CLIENT_TOKEN", Server: mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{u.Host}, Origins: []string{u.Scheme + "://" + u.Host}}}}, nil
}
