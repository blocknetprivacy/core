package params

// PrefersBranch reports whether a node on a chain with curWork at curHeight, whose
// tip has difficulty curTipDifficulty, should switch to a branch with newWork at
// newHeight.
//
// The branch must have more cumulative work and either be longer or carry more
// than half a block of extra work at the current tip's difficulty. Branches of the
// same length differ in work mostly through their block timestamps, which miners
// choose, so a small difference keeps the branch the node saw first. Any block
// found on that branch then outweighs the other one under either rule.
func PrefersBranch(curWork, curHeight, curTipDifficulty, newWork, newHeight uint64) bool {
	if newWork <= curWork {
		return false
	}
	if newHeight > curHeight {
		return true
	}
	return newWork-curWork > curTipDifficulty/2
}
