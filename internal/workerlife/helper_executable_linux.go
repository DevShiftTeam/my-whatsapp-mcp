//go:build linux

package workerlife

import (
	"context"
	"os"
	"strconv"
)

func helperExecutable(_ context.Context, _ Config, _ *os.File, childFD int) (string, ExecutableLease, error) {
	return "/dev/fd/" + strconv.Itoa(childFD), nil, nil
}
func ValidateExecutableConfig(Config) bool { return true }
