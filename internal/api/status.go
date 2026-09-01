package api

import (
	"net/http"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/plugins"
)

// handleStatus reports the §7.1 gateway_status surface: store stats, disk
// usage, retention, uptime, plugin list, and the routable aliases.
func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.store.Status(r.Context())
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "INTERNAL", "store status: "+err.Error())
		return
	}
	cfg := a.mgr.Get()
	disabled := 0
	for _, inst := range cfg.Instances {
		if inst.Disabled {
			disabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"store_path":         st.StorePath,
		"session_count":      st.SessionCount,
		"request_count":      st.RequestCount,
		"tool_call_count":    st.ToolCallCount,
		"failure_count":      st.FailureCount,
		"disk_usage_bytes":   st.DiskUsageBytes,
		"retention_days":     st.RetentionDays,
		"uptime_seconds":     int64(time.Since(a.startedAt).Seconds()),
		"listen_addrs":       cfg.Addrs(),
		"plugins":            plugins.Known(),
		"aliases":            cfg.AliasList(),
		"disabled_instances": disabled,
	})
}
