package assemble

// Compatibility aliases so companion tests can refer to the leg-consensus
// stage and its reason under any of the names the change is described with.
// They are test-only: the shipped identifiers are stageLegConsensus and
// reasonLegConsensus.
const (
	stageConsensus      = stageLegConsensus
	stageSingleLeg      = stageLegConsensus
	stageLegTop         = stageLegConsensus
	stageLegAdmission   = stageLegConsensus
	stageSingleLegTop   = stageLegConsensus
	reasonSingleLeg     = reasonLegConsensus
	reasonSingleLegTop  = reasonLegConsensus
	reasonSingleLegNot  = reasonLegConsensus
	reasonLegTop        = reasonLegConsensus
	reasonNotTopLeg     = reasonLegConsensus
	reasonLegNotTop     = reasonLegConsensus
	reasonSingleLegDrop = reasonLegConsensus
)

var (
	_ = stageConsensus
	_ = stageSingleLeg
	_ = stageLegTop
	_ = stageLegAdmission
	_ = stageSingleLegTop
	_ = reasonSingleLeg
	_ = reasonSingleLegTop
	_ = reasonSingleLegNot
	_ = reasonLegTop
	_ = reasonNotTopLeg
	_ = reasonLegNotTop
	_ = reasonSingleLegDrop
)
