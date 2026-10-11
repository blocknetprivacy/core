package main

const (
	networkUpgradeDate     = "2027-02-16"
	networkUpgradeDeadline = "2028-02-16"
	networkUpgradeNotice   = "Blocknet plans a network upgrade around 2027-02-16. After it, you will have 1 year to move your coins to the new format, until about 2028-02-16. Coins not moved by then cannot be spent. The wallet will guide you."
	explorerUpgradeBanner  = `<div style="border:1px solid #fa0;color:#fa0;padding:10px 14px;margin:0 0 20px;font-size:13px;line-height:1.5"><b>Network upgrade notice:</b> ` + networkUpgradeNotice + `</div>`
)

type networkUpgradeInfo struct {
	Date              string `json:"date"`
	MigrationDeadline string `json:"migration_deadline"`
	Notice            string `json:"notice"`
}

func currentNetworkUpgrade() networkUpgradeInfo {
	return networkUpgradeInfo{Date: networkUpgradeDate, MigrationDeadline: networkUpgradeDeadline, Notice: networkUpgradeNotice}
}
