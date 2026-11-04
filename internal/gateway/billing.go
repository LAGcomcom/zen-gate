package gateway

import (
	"strings"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// Billing classification is per published model id, not per provider: one
// supplier can publish both a free tier and paid models, and a lane model and
// a custom-provider mirror of the same weights can sit in different tiers.
//
// The rule is deliberately conservative. A free-lane id is free because it is
// served without any user key. A custom provider's tier is whatever its node
// list declared; an absent or unrecognisable declaration is "unknown", never
// guessed as free (a wrong "free" label is how someone gets a surprise bill).
func billingForModel(id string, freeLane bool, cfg *store.Config) string {
	if freeLane {
		return store.BillingFree
	}
	if i := strings.IndexByte(id, '/'); i > 0 {
		pid := id[:i]
		for i := range cfg.Providers {
			if cfg.Providers[i].ID != pid {
				continue
			}
			if b, err := store.NormalizeBilling(cfg.Providers[i].Billing); err == nil {
				return b
			}
			return store.BillingUnknown
		}
	}
	return store.BillingUnknown
}

// laneBilling uses the lane's own free/paid test for curated ids. A model the
// free-id rules do not recognise is left unknown: the catalog can carry ids
// whose pricing we have not established.
func laneBilling(id string) string {
	if lane.IsFreeLane(id) {
		return store.BillingFree
	}
	return store.BillingUnknown
}
