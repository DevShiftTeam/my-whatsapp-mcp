package app

// Deferred recovery retains immutable trusted composition, never caller slices.
func copyApplicationConfig(cfg Config) Config {
	cfg.RebuildAncestorLockFDs = append([]int(nil), cfg.RebuildAncestorLockFDs...)
	if cfg.RebuildSourceLockFD != nil {
		fd := *cfg.RebuildSourceLockFD
		cfg.RebuildSourceLockFD = &fd
	}
	if cfg.Snapshot != nil {
		v := *cfg.Snapshot
		cfg.Snapshot = &v
	}
	cfg.ReservedRemoteOrigins = append([]string(nil), cfg.ReservedRemoteOrigins...)
	if cfg.Transcription != nil {
		value := *cfg.Transcription
		cfg.Transcription = &value
	}
	if cfg.Native != nil {
		value := *cfg.Native
		value.WorkerLifetime.Args = append([]string(nil), value.WorkerLifetime.Args...)
		value.WorkerLifetime.Environment = append([]string(nil), value.WorkerLifetime.Environment...)
		if value.IndexVersion != nil {
			version := *value.IndexVersion
			value.IndexVersion = &version
		}
		cfg.Native = &value
	}
	if cfg.Media != nil {
		value := *cfg.Media
		value.WorkerLifetime.Args = append([]string(nil), value.WorkerLifetime.Args...)
		value.WorkerLifetime.Environment = append([]string(nil), value.WorkerLifetime.Environment...)
		value.ExtractionVersions = append(value.ExtractionVersions[:0:0], value.ExtractionVersions...)
		cfg.Media = &value
	}
	if cfg.OriginalBudgetBytes != nil {
		value := *cfg.OriginalBudgetBytes
		cfg.OriginalBudgetBytes = &value
	}
	if cfg.MCP != nil {
		value := copyMCPConfig(cfg.MCP)
		cfg.MCP = &value
	}
	return cfg
}
