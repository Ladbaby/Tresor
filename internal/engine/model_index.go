package engine

import "tresor/internal/store"

// downstreamModelIndex shares attribution and disabled-provider lookups only.
// Each endpoint retains its own visibility, normalization and dedup policy.
func downstreamModelIndex(downstreams []store.Downstream) (names map[string]string, disabled map[string]struct{}) {
	names = make(map[string]string, len(downstreams))
	disabled = make(map[string]struct{}, len(downstreams))
	for _, ds := range downstreams {
		names[ds.ID] = ds.Name
		if !ds.IsEnabled {
			disabled[ds.ID] = struct{}{}
		}
	}
	return names, disabled
}
