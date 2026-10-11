package main

import "github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"

func isWorkerHolder(arguments []string) bool {
	return len(arguments) == 1 && arguments[0] == workerlife.HolderModeArgument
}
func runWorkerHolder() int {
	if err := workerlife.RunInherited(); err != nil {
		return 1
	}
	return 0
}
