//go:build !darwin && !linux

package workerlife

func ValidateExecutableConfig(Config) bool { return false }
