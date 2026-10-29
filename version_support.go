package main

const (
	releaseHeight        = 70000
	versionSupportBlocks = 51840
	outdatedNotice       = "This version of Blocknet is more than 6 months old, so a newer version is almost certainly out. Nothing is broken and your coins are safe. Run blocknet upgrade to update, and keep auto-upgrade on so this does not happen again."
)

func updateNotice(height uint64) string {
	if height >= releaseHeight+versionSupportBlocks {
		return outdatedNotice
	}
	return ""
}
