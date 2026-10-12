package clientsetup

import "strings"

// DockerRunner is trusted server composition, never a client-selected command.
type DockerRunner struct {
	Container string `json:"container"`
}

// DockerStdioCommand returns a literal argv bridge. The host Docker executable
// must be on the client's PATH; credentials and the Unix socket stay inside the
// container. No shell, TTY, host file read or daemon/socket mount is involved.
func DockerStdioCommand(container, configPath string) (string, []string, error) {
	if !validDockerContainer(container) || !safeText(configPath, 4096) || !validConfigPath(configPath) || strings.ContainsAny(configPath, ";&|`$<>()") {
		return "", nil, ErrInvalidConfig
	}
	return "docker", []string{"exec", "-i", container, "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", configPath}, nil
}

// Keep the CLI's bounded profile/container grammar: ASCII alphanumeric first,
// then ASCII alphanumeric, dash, underscore or dot, at most 128 bytes.
func validDockerContainer(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '-' || r == '_' || r == '.')) {
			return false
		}
	}
	return true
}
