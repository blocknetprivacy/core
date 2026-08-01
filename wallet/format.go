package wallet

import (
	"fmt"
	"strings"
)

// FormatBNT renders atomic units (1 BNT = 100,000,000, 8 decimals) as a BNT string.
func FormatBNT(atomicUnits uint64) string {
	whole := atomicUnits / 100_000_000
	frac := atomicUnits % 100_000_000
	if frac == 0 {
		return fmt.Sprintf("%d BNT", whole)
	}
	fracStr := strings.TrimRight(fmt.Sprintf("%08d", frac), "0")
	return fmt.Sprintf("%d.%s BNT", whole, fracStr)
}
