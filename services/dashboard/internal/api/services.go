package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/otomo-live/otomo/services/dashboard/internal/health"
)

// servicesPath is the external path for GET /services. It is stated once so the route
// table and the handler seam cannot drift apart.
const servicesPath = "/api/admin/dashboard/services"

// servicesPattern is the ServeMux pattern for servicesPath, compared by For while the
// mux is assembled.
const servicesPattern = http.MethodGet + " " + servicesPath

// listServices answers the overview's service list: every configured target and what the
// last probe found. Prometheus scrape fields land in a later ticket; this response is
// only what the prober knows. Snapshot is already sorted, but the handler sorts its own
// copy so the wire order is a property of this endpoint regardless of the provider.
func (h *Handlers) listServices(w http.ResponseWriter, r *http.Request) {
	var statuses []health.Status
	if h.Services != nil {
		statuses = append(statuses, h.Services()...)
	}
	if statuses == nil {
		statuses = []health.Status{}
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Services []health.Status `json:"services"`
	}{Services: statuses})
}
