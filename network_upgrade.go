package main

const (
	networkUpgradeDate     = "2027-02-16"
	networkUpgradeDeadline = "2028-02-16"
	networkUpgradeNotice   = "Blocknet plans a network upgrade around 2027-02-16. After it, you will have 1 year to move your coins to the new format, until about 2028-02-16. Coins not moved by then cannot be spent. The wallet will guide you."
	explorerUpgradeBanner  = `<div class="notice"><div class="wrap"><b>upgrade notice</b><span class="nl">` + networkUpgradeNotice + `</span><span class="ns">Planned ~` + networkUpgradeDate + `. You then have 1 year to move coins. <a href="https://blocknetcrypto.com/#faraday">details</a></span></div></div>`
)

type networkUpgradeInfo struct {
	Date              string `json:"date"`
	MigrationDeadline string `json:"migration_deadline"`
	Notice            string `json:"notice"`
}

func currentNetworkUpgrade() networkUpgradeInfo {
	return networkUpgradeInfo{Date: networkUpgradeDate, MigrationDeadline: networkUpgradeDeadline, Notice: networkUpgradeNotice}
}
