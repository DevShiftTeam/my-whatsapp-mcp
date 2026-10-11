package app

import "github.com/Bodokh/my-whatsapp-mcp/internal/archive"

func (r *resources) progress(status *archive.StatusSnapshot) {
	if r.recoveryPending.Load() {
		status.Media = archive.Progress{State: "pending", Code: "extraction_recovery_pending"}
		status.Semantic = archive.Progress{State: "pending", Code: "extraction_recovery_pending"}
		return
	}
	if !r.nativeConfigured {
		status.Semantic = archive.Progress{State: "not_configured"}
	} else if r.native == nil {
		status.Semantic.State = "unavailable"
		status.Semantic.Code = "profile_not_accepted"
	} else {
		r.index.mu.Lock()
		if r.index.service == nil {
			status.Semantic.State = "pending"
			status.Semantic.Code = "index_not_configured"
		} else {
			state := r.index.service.Snapshot()
			status.Semantic.State = archive.ProgressState(state.State)
			status.Semantic.Code = state.Code
		}
		r.index.mu.Unlock()
	}
	if !r.mediaConfigured {
		status.Media = archive.Progress{State: "not_configured"}
	} else if r.extraction == nil {
		status.Media.State = "unavailable"
		status.Media.Code = "media_not_accepted"
	} else if status.Media.State == "" || status.Media.State == "not_configured" {
		status.Media.State = "pending"
		status.Media.Code = "media_pending"
	}
	if r.mediaJobs != nil {
		r.mediaJobs.mu.Lock()
		if r.mediaJobs.code != "" {
			status.Media.Code = r.mediaJobs.code
		}
		r.mediaJobs.mu.Unlock()
	}
	if r.primaryJobs != nil {
		r.primaryJobs.mu.Lock()
		if r.primaryJobs.code != "" {
			status.Media.Code = r.primaryJobs.code
		}
		r.primaryJobs.mu.Unlock()
	}
}
