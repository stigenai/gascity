package main

// routedDemandStrandedEventType is the wire event type emitted when a
// gc.routed_to demand target is detected stranded (FR-4). Detection and
// emission are implemented in the GREEN step; this compile-time stub only
// unblocks the RED-step test files.
const routedDemandStrandedEventType = "routed_demand.stranded"

// poolDemandWispMetadataKey marks a molecule/wisp bead as carrying
// order-dispatch pool demand (FR-5), the one narrow additive exception to
// readyExcludeTypes for stranded-demand detection purposes.
const poolDemandWispMetadataKey = "gc.pool_demand_wisp"

func poolDemandMetadataPair() map[string]string {
	return map[string]string{poolDemandWispMetadataKey: boolMetadata(true)}
}
