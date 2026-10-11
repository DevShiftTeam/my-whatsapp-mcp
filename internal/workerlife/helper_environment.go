package workerlife

import "strings"

// WithoutLoaderOverrides composes borrowed environments while removing DYLD_*
// and LD_PRELOAD, LD_LIBRARY_PATH and LD_AUDIT. It preserves role/task values
// and ordering without changing either input or adding runtime thread settings.
func WithoutLoaderOverrides(ambient, configured []string) []string {
	result := make([]string, 0, len(ambient)+len(configured)+1)
	for _, group := range [][]string{ambient, configured} {
		for _, value := range group {
			key, _, _ := strings.Cut(value, "=")
			if strings.HasPrefix(key, "DYLD_") || key == "LD_PRELOAD" || key == "LD_LIBRARY_PATH" || key == "LD_AUDIT" {
				continue
			}
			result = append(result, value)
		}
	}
	return result
}

func helperEnvironment(ambient, configured []string) []string {
	return append(WithoutLoaderOverrides(ambient, configured), "GOMAXPROCS=1")
}
