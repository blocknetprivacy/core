package main

import (
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"blocknet/wallet"
)

func (c *CLI) cmdBalance() {
	height := c.daemon.Chain().Height()
	spendable := c.wallet.SpendableBalance(height)
	pending := c.wallet.PendingBalance(height)
	pendingUnconfirmed := c.wallet.PendingUnconfirmedBalance()
	total, unspent := c.wallet.OutputCount()

	fmt.Printf("\n%s\n", c.sectionHead("Balance"))
	fmt.Printf("  spendable:  %s\n", formatAmount(spendable))
	fmt.Printf("  confirming: %s\n", formatAmount(pending))
	if pendingUnconfirmed > 0 {
		eta := time.Duration(wallet.SafeConfirmations+1) * wallet.EstimatedBlockInterval
		fmt.Printf("  pending:    %s (est unlock ~%s)\n", formatAmount(pendingUnconfirmed), eta.Round(time.Minute))
	}
	fmt.Printf("  total:      %s\n", formatAmount(spendable+pending))
	fmt.Printf("  outputs:    %d unspent", unspent)
	if total > unspent {
		fmt.Printf(", %d spent", total-unspent)
	}
	fmt.Println()
}

func (c *CLI) cmdAddress() {
	fmt.Printf("\n%s\n", c.sectionHead("Address"))
	fmt.Println()
	fmt.Printf("  %s\n", c.wallet.Address())
	fmt.Println()
	url := "https://blocknet.id"
	if !c.noColor {
		url = "\033[4m\033[38;2;170;255;0mhttps://blocknet.id\033[0m"
	}
	fmt.Printf("  Get a short name like @name or $name at %s\n", url)
}

func (c *CLI) cmdSend(args []string) error {
	// View-only wallets cannot send
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("cannot send from a view-only wallet")
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: send <address> <amount> [memo|hex:<memo_hex>]")
	}

	// Parse optional memo. Plain text is UTF-8 bytes. Hex can be passed as hex:<hex>.
	var memo []byte
	if len(args) >= 3 {
		// strings.Fields() doesn't preserve quoting, so join the remainder back
		// together to support memos with spaces.
		raw := strings.TrimSpace(strings.Join(args[2:], " "))
		if strings.HasPrefix(raw, "hex:") {
			decoded, err := hex.DecodeString(strings.TrimPrefix(raw, "hex:"))
			if err != nil {
				return fmt.Errorf("invalid memo hex")
			}
			memo = decoded
		} else {
			// Best-effort strip of single/double quotes for UX when user types:
			// send <addr> <amt> "memo with spaces"
			if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
				raw = raw[1 : len(raw)-1]
			}
			memo = []byte(raw)
		}
		if len(memo) > wallet.MemoSize-4 {
			return fmt.Errorf("memo too long: max %d bytes", wallet.MemoSize-4)
		}
	}

	// Parse recipient address (strip control characters from copy-paste)
	recipientInput := sanitizeInput(args[0])
	recipientAddr, resolvedInfo, err := resolveRecipientAddress(recipientInput)
	if err != nil {
		return fmt.Errorf("invalid recipient: %w", err)
	}
	if resolvedInfo != nil && resolvedInfo.Verified {
		if c.noColor {
			fmt.Printf("  Resolved %s -> %s [verified]\n", recipientInput, recipientAddr)
		} else {
			fmt.Printf("  Resolved %s -> %s \033[38;2;170;255;0m✓ verified\033[0m\n", recipientInput, recipientAddr)
		}
	}
	spendPub, viewPub, err := wallet.ParseAddress(recipientAddr)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	// Parse amount
	sendAll := strings.EqualFold(args[1], "all")
	var amount uint64
	if !sendAll {
		amount, err = parseAmount(args[1])
		if err != nil {
			return fmt.Errorf("invalid amount: %w", err)
		}
	}

	// Check spendable balance (excludes immature coinbase and unconfirmed)
	height := c.daemon.Chain().Height()
	spendable := c.wallet.SpendableBalance(height)
	if sendAll {
		if spendable == 0 {
			return fmt.Errorf("no spendable balance")
		}
	} else if spendable < amount {
		pending := c.wallet.PendingBalance(height)
		if pending > 0 {
			return fmt.Errorf("insufficient spendable balance: have %s spendable + %s pending, need %s",
				formatAmount(spendable), formatAmount(pending), formatAmount(amount))
		}
		return fmt.Errorf("insufficient balance: have %s, need %s",
			formatAmount(spendable), formatAmount(amount))
	}

	fmt.Printf("\n%s\n", c.sectionHead("Send"))
	fmt.Println("  Building transaction...")

	recipient := wallet.Recipient{
		SpendPubKey: spendPub,
		ViewPubKey:  viewPub,
		Amount:      amount,
		Memo:        memo,
	}

	builder := c.createTxBuilder()
	chainHeight := c.daemon.Chain().Height()
	var result *wallet.TransferResult
	if sendAll {
		result, err = builder.TransferAll(recipient, 10, chainHeight)
	} else {
		result, err = builder.Transfer([]wallet.Recipient{recipient}, 10, chainHeight)
	}
	if err != nil {
		return fmt.Errorf("failed to build transaction: %w", err)
	}
	if sendAll {
		for _, spent := range result.SpentOutputs {
			amount += spent.Amount
		}
		amount -= result.Fee
	}

	// Confirm
	recipientLabel := recipientAddr
	if resolvedInfo != nil {
		recipientLabel = recipientInput
	}
	fmt.Printf("\n  Send %s to %s?\n", formatAmount(amount), recipientLabel)
	fmt.Printf("  Fee:     %s\n", formatAmount(result.Fee))
	if resolvedInfo != nil {
		fmt.Printf("  Address: %s\n", recipientAddr)
	}
	if result.Change > 0 {
		blocksUntilSpendable := uint64(wallet.SafeConfirmations + 1)
		arrivalBlock := chainHeight + blocksUntilSpendable
		eta := time.Duration(blocksUntilSpendable) * wallet.EstimatedBlockInterval
		fmt.Printf("  Change:  %s — spendable in ~%d blocks (block %d, ~%s from now)\n",
			formatAmount(result.Change), blocksUntilSpendable, arrivalBlock, formatDuration(eta))
	}
	if len(memo) > 0 {
		if memoText, ok := memoTextIfPrintable(memo); ok {
			fmt.Printf("  Memo:    %s\n", strconv.QuoteToASCII(memoText))
		} else {
			fmt.Printf("  Memo:    %s\n", strings.ToUpper(hex.EncodeToString(memo)))
		}
	}
	fmt.Print("  Confirm [y/N]: ")

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		c.wallet.ReleaseInputLease(result.InputLease)
		fmt.Println("  Cancelled")
		return nil
	}

	// Submit to mempool via Dandelion++
	if err := c.daemon.SubmitTransaction(result.TxData); err != nil {
		c.wallet.ReleaseInputLease(result.InputLease)
		return fmt.Errorf("failed to submit transaction: %w", err)
	}

	// Mark outputs as spent
	for _, spent := range result.SpentOutputs {
		c.wallet.MarkSpent(spent.OneTimePubKey, c.daemon.Chain().Height())
	}

	// Record send for history tracking
	c.wallet.RecordSend(&wallet.SendRecord{
		TxID:      result.TxID,
		Timestamp: time.Now().Unix(),
		Recipients: []wallet.SendRecipient{{
			Address: recipientLabel,
			Amount:  amount,
			Memo:    memo,
		}},
		Fee:         result.Fee,
		BlockHeight: c.daemon.Chain().Height(),
	})
	if result.Change > 0 {
		// UX: surface expected change immediately until it is confirmed/scanned.
		c.wallet.AddPendingCredit(result.TxID, result.Change)
	}
	if err := c.wallet.Save(); err != nil {
		fmt.Printf("  Warning: wallet persistence failed: %v\n", err)
	}

	txHex := fmt.Sprintf("%x", result.TxID)
	fmt.Printf("  Sent: %s\n", txHex)
	fmt.Printf("  Explorer: https://explorer.blocknetcrypto.com/tx/%s\n", txHex)

	return nil
}

func (c *CLI) cmdSign() error {
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("view-only wallet cannot sign")
	}

	fmt.Printf("\n%s\n", c.sectionHead("Sign"))
	fmt.Println("  Enter the text to sign, press ENTER when you're done.")
	fmt.Print("\n> ")

	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	message := strings.TrimSpace(line)
	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}
	if len(message) > 1024 {
		return fmt.Errorf("message must be <= 1024 bytes")
	}

	keys := c.wallet.Keys()
	sig, err := SchnorrSign(keys.SpendPrivKey[:], []byte(message))
	if err != nil {
		return fmt.Errorf("signing failed: %w", err)
	}

	fmt.Printf("\n  %s\n", hex.EncodeToString(sig))
	return nil
}

func (c *CLI) cmdVerifyMsg() error {
	fmt.Printf("\n%s\n", c.sectionHead("Verify"))
	fmt.Println("  Enter the address:")
	fmt.Print("\n> ")
	addrLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	address := strings.TrimSpace(addrLine)
	if address == "" {
		return fmt.Errorf("address cannot be empty")
	}

	fmt.Println("  Enter the message that was signed:")
	fmt.Print("\n> ")
	msgLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	message := strings.TrimSpace(msgLine)
	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}

	fmt.Println("  Enter the signature (hex):")
	fmt.Print("\n> ")
	sigLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read input: %w", err)
	}
	sigHex := strings.TrimSpace(sigLine)

	spendPub, _, err := wallet.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}

	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != 64 {
		return fmt.Errorf("invalid signature: must be 64 bytes hex-encoded")
	}

	if err := SchnorrVerify(spendPub[:], []byte(message), sigBytes); err != nil {
		fmt.Printf("\n  %s\n", c.errorHead("Signature is INVALID"))
		return nil
	}

	fmt.Printf("\n  %s\n", c.sectionHead("Signature is VALID"))
	return nil
}

func (c *CLI) cmdHistory() {
	// Get all outputs (both spent and unspent)
	outputs := c.wallet.AllOutputs()
	sendRecords := c.wallet.SendRecords()

	if len(outputs) == 0 && len(sendRecords) == 0 {
		fmt.Printf("\n%s\n", c.sectionHead("History"))
		fmt.Println("  No transactions yet")
		return
	}

	// Color codes
	green := "\033[38;2;170;255;0m" // #AAFF00 - incoming
	red := "\033[38;2;255;68;68m"   // #FF4444 - outgoing
	dim := "\033[2;33m"             // dim yellow - pending
	reset := "\033[0m"

	if c.noColor {
		green = ""
		red = ""
		dim = ""
		reset = ""
	}

	// Build history events (both IN and OUT)
	type historyEvent struct {
		timestamp int64
		direction string
		amount    uint64
		height    uint64
		color     string
		txHash    [32]byte
		memo      []byte
		pending   bool
	}

	var events []historyEvent

	// Add incoming events
	for _, out := range outputs {
		block := c.daemon.Chain().GetBlockByHeight(out.BlockHeight)
		if block == nil {
			continue
		}
		events = append(events, historyEvent{
			timestamp: block.Header.Timestamp,
			direction: "IN",
			amount:    out.Amount,
			height:    out.BlockHeight,
			color:     green,
			txHash:    out.TxID,
			memo:      out.Memo,
		})
	}

	// Add outgoing events from the wallet's persisted send history.
	// This is the authoritative source for outbound txids; owned outputs only
	// contain the txid that created them (not the txid that spent them).
	seenTxIDs := make(map[[32]byte]bool)
	for _, sendRecord := range sendRecords {
		if sendRecord == nil || seenTxIDs[sendRecord.TxID] {
			continue
		}

		pending := sendRecord.BlockHeight == 0

		// Prefer chain timestamp when the block is available, otherwise fall back
		// to the local timestamp captured at send time.
		ts := sendRecord.Timestamp
		if sendRecord.BlockHeight > 0 {
			if block := c.daemon.Chain().GetBlockByHeight(sendRecord.BlockHeight); block != nil {
				ts = block.Header.Timestamp
			}
		}

		color := red
		if pending {
			color = dim
		}

		var firstMemo []byte
		recipients := sendRecord.GetRecipients()
		if len(recipients) > 0 {
			firstMemo = recipients[0].Memo
		}

		events = append(events, historyEvent{
			timestamp: ts,
			direction: "OUT",
			amount:    sendRecord.TotalAmount(),
			height:    sendRecord.BlockHeight,
			color:     color,
			txHash:    sendRecord.TxID,
			memo:      firstMemo,
			pending:   pending,
		})
		seenTxIDs[sendRecord.TxID] = true
	}

	// Sort by timestamp (oldest first)
	sort.Slice(events, func(i, j int) bool {
		return events[i].timestamp < events[j].timestamp
	})

	fmt.Printf("\n%s\n", c.sectionHead("History"))
	for _, evt := range events {
		tm := time.Unix(evt.timestamp, 0)
		dateStr := tm.Format("060102-15:04:05")

		amountStr := formatAmount(evt.amount)
		if evt.amount == 0 && evt.direction == "OUT" {
			amountStr = "??? BNT"
		}

		pendingTag := ""
		if evt.pending {
			pendingTag = " [mempool]"
		}

		memoStr := ""
		if len(evt.memo) > 0 {
			memoHex := hex.EncodeToString(evt.memo)
			if memoText, ok := memoTextIfPrintable(evt.memo); ok {
				memoStr = "\n    memo: " + strconv.QuoteToASCII(memoText)
			} else {
				memoStr = "\n    memo: " + memoHex
			}
		}

		fmt.Printf("  %s %s%-3s%s %-16s %x%s%s\n",
			dateStr,
			evt.color,
			evt.direction,
			reset,
			amountStr,
			evt.txHash,
			pendingTag,
			memoStr,
		)
	}
}

func (c *CLI) cmdOutputs(args []string) {
	outputs := c.wallet.AllOutputs()
	chainHeight := c.daemon.Chain().Height()
	walletHeight := c.wallet.SyncedHeight()
	filter := "all"
	detailIndex := -1
	showByTx := false
	var txIDFilter [32]byte
	txIndexFilter := -1

	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "spent", "unspent", "pending":
			filter = strings.ToLower(args[0])
			if len(args) == 2 {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 {
					fmt.Println("  Usage: outputs [spent|unspent|pending] [index]")
					fmt.Println("         outputs tx <txid>")
					fmt.Println("         outputs tx <txid>:<index>")
					return
				}
				detailIndex = n
			} else if len(args) > 2 {
				fmt.Println("  Usage: outputs [spent|unspent|pending] [index]")
				fmt.Println("         outputs tx <txid>")
				fmt.Println("         outputs tx <txid>:<index>")
				return
			}
		case "tx":
			if len(args) != 2 {
				fmt.Println("  Usage: outputs tx <txid>")
				fmt.Println("         outputs tx <txid>:<index>")
				return
			}
			parts := strings.SplitN(strings.TrimSpace(args[1]), ":", 2)
			rawTx := strings.TrimSpace(parts[0])
			decoded, err := hex.DecodeString(rawTx)
			if err != nil || len(decoded) != 32 {
				fmt.Println("  Invalid txid. Expected 64 hex characters.")
				return
			}
			copy(txIDFilter[:], decoded)
			showByTx = true
			if len(parts) == 2 {
				n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil || n < 0 {
					fmt.Println("  Invalid tx output index. Use a non-negative number.")
					return
				}
				txIndexFilter = n
			}
		default:
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 || len(args) > 1 {
				fmt.Println("  Usage: outputs [spent|unspent|pending] [index]")
				fmt.Println("         outputs tx <txid>")
				fmt.Println("         outputs tx <txid>:<index>")
				return
			}
			detailIndex = n
		}
	}

	fmt.Printf("\n%s\n", c.sectionHead("Outputs"))
	if len(outputs) == 0 && !showByTx {
		if walletHeight < chainHeight {
			fmt.Printf("  No outputs found yet.\n")
			fmt.Printf("  Wallet scan is still catching up (%d/%d).\n", walletHeight, chainHeight)
			fmt.Println("  Try: sync")
			return
		}
		fmt.Println("  No outputs found in this wallet yet.")
		fmt.Println("  Receive funds, then run sync if needed.")
		return
	}

	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].BlockHeight == outputs[j].BlockHeight {
			if outputs[i].TxID == outputs[j].TxID {
				return outputs[i].OutputIndex < outputs[j].OutputIndex
			}
			return strings.Compare(fmt.Sprintf("%x", outputs[i].TxID), fmt.Sprintf("%x", outputs[j].TxID)) < 0
		}
		return outputs[i].BlockHeight < outputs[j].BlockHeight
	})

	statusLabel := func(status string) string {
		if c.noColor {
			return status
		}
		switch status {
		case "unspent":
			return "\033[38;2;170;255;0munspent\033[0m"
		case "spent":
			return "\033[38;2;255;68;68mspent\033[0m"
		case "pending":
			return "\033[38;2;255;170;0mpending\033[0m"
		default:
			return status
		}
	}

	outputStatus := func(out *wallet.OwnedOutput) string {
		if out.Spent {
			return "spent"
		}
		if wallet.IsOutputMature(out, chainHeight) {
			return "unspent"
		}
		return "pending"
	}

	matchesFilter := func(out *wallet.OwnedOutput) bool {
		if filter == "all" {
			return true
		}
		return outputStatus(out) == filter
	}

	filtered := make([]*wallet.OwnedOutput, 0, len(outputs))
	for _, out := range outputs {
		if matchesFilter(out) {
			filtered = append(filtered, out)
		}
	}

	printDetails := func(pos int, out *wallet.OwnedOutput) {
		status := outputStatus(out)
		outType := "regular"
		if out.IsCoinbase {
			outType = "coinbase"
		}

		conf := uint64(0)
		if chainHeight >= out.BlockHeight {
			conf = chainHeight - out.BlockHeight
		}

		fmt.Printf("  #%d\n", pos)
		fmt.Printf("    status:       %s\n", statusLabel(status))
		fmt.Printf("    amount:       %s\n", formatAmount(out.Amount))
		fmt.Printf("    type:         %s\n", outType)
		fmt.Printf("    confirmations:%d\n", conf)
		fmt.Printf("    block:        %d\n", out.BlockHeight)
		fmt.Printf("    tx output:    %x:%d\n", out.TxID, out.OutputIndex)
		fmt.Printf("    one-time pub: %x\n", out.OneTimePubKey)
		fmt.Printf("    commitment:   %x\n", out.Commitment)
		if out.Spent {
			fmt.Printf("    spent block:  %d\n", out.SpentHeight)
		}
		if len(out.Memo) > 0 {
			if memoText, ok := memoTextIfPrintable(out.Memo); ok {
				fmt.Printf("    memo:         %s\n", strconv.QuoteToASCII(memoText))
			} else {
				fmt.Printf("    memo:         %s\n", strings.ToUpper(hex.EncodeToString(out.Memo)))
			}
		}
	}

	if showByTx {
		matches := make([]*wallet.OwnedOutput, 0)
		for _, out := range outputs {
			if out.TxID != txIDFilter {
				continue
			}
			if txIndexFilter >= 0 && out.OutputIndex != txIndexFilter {
				continue
			}
			if !matchesFilter(out) {
				continue
			}
			matches = append(matches, out)
		}
		if len(matches) == 0 {
			fmt.Println("  No matching outputs owned by this wallet for that tx.")
			return
		}
		for i, out := range matches {
			if i > 0 {
				fmt.Println()
			}
			printDetails(i+1, out)
		}
		return
	}

	if len(filtered) == 0 {
		if filter == "all" {
			if walletHeight < chainHeight {
				fmt.Printf("  No outputs found yet.\n")
				fmt.Printf("  Wallet scan is still catching up (%d/%d).\n", walletHeight, chainHeight)
				fmt.Println("  Try: sync")
				return
			}
			fmt.Println("  No outputs found in this wallet yet.")
			fmt.Println("  Receive funds, then run sync if needed.")
			return
		}
		fmt.Printf("  No %s outputs found.\n", filter)
		return
	}

	if detailIndex > 0 {
		if detailIndex > len(filtered) {
			fmt.Printf("  Output index %d is out of range (1-%d).\n", detailIndex, len(filtered))
			return
		}
		printDetails(detailIndex, filtered[detailIndex-1])
		return
	}

	for i, out := range filtered {
		status := outputStatus(out)
		outType := "regular"
		if out.IsCoinbase {
			outType = "coinbase"
		}

		conf := uint64(0)
		if chainHeight >= out.BlockHeight {
			conf = chainHeight - out.BlockHeight
		}

		fmt.Printf("  #%d  %-12s %-8s conf: %d\n", i+1, statusLabel(status), outType, conf)
		fmt.Printf("      amount: %s\n", formatAmount(out.Amount))
		fmt.Printf("      block:  %d  tx: %x:%d\n", out.BlockHeight, out.TxID, out.OutputIndex)
	}
}

func memoTextIfPrintable(b []byte) (string, bool) {
	// Trim trailing NUL padding (common for fixed-size memo fields).
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	b = b[:end]
	if len(b) == 0 {
		return "", false
	}

	if !utf8.Valid(b) {
		return "", false
	}

	s := string(b)
	for _, r := range s {
		// Avoid breaking the one-line history output.
		if r == '\n' || r == '\r' {
			return "", false
		}
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return s, true
}

func (c *CLI) cmdSync() {
	chainHeight := c.daemon.Chain().Height()
	walletHeight := c.wallet.SyncedHeight()

	fmt.Printf("\n%s\n", c.sectionHead("Sync"))
	fmt.Printf("  Known blocks:   %d\n", chainHeight)
	fmt.Printf("  Blocks scanned: %d\n", walletHeight)

	if walletHeight >= chainHeight {
		return
	}

	fmt.Printf("  Scanning %d blocks...\n", chainHeight-walletHeight)

	// Snapshot blocks under a single chain read lock so we don't pay RWMutex
	// writer-preference overhead per height while the node is ingesting blocks.
	blocks := c.daemon.Chain().GetBlocksByHeightRange(walletHeight+1, chainHeight)

	scannedTo := walletHeight
	for _, block := range blocks {
		if block == nil {
			break
		}

		blockData := blockToScanData(block)
		found, spent := c.scanner.ScanBlock(blockData)

		h := block.Header.Height
		scannedTo = h
		if found > 0 || spent > 0 {
			fmt.Printf("    Block %d: +%d outputs, %d spent\n", h, found, spent)
		}
	}

	if scannedTo > walletHeight {
		c.wallet.SetSyncedHeight(scannedTo)
		fmt.Printf("  Wallet synced to height %d\n", scannedTo)
	}
}

func (c *CLI) cmdSeed() error {
	amber := "\033[38;2;255;170;0m"
	rst := "\033[0m"
	if c.noColor {
		amber = ""
		rst = ""
	}
	fmt.Printf("\n%s%s%s\n", amber, "# Seed", rst)
	fmt.Printf("  %sWARNING: Your recovery seed controls all funds.%s\n", amber, rst)
	fmt.Printf("  %sAnyone with this seed can steal your coins.%s\n", amber, rst)
	fmt.Printf("  %sNever share it. Never enter it online.%s\n", amber, rst)
	fmt.Print("\n  Show recovery seed? [y/N]: ")

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		return nil
	}

	mnemonic, err := c.wallet.Mnemonic()
	if err != nil {
		return fmt.Errorf("failed to read mnemonic: %w", err)
	}
	if mnemonic == "" {
		fmt.Println("  No recovery seed available (wallet may predate BIP39 support)")
		return nil
	}

	fmt.Println()
	words := strings.Fields(mnemonic)
	for i := 0; i < len(words); i += 4 {
		end := i + 4
		if end > len(words) {
			end = len(words)
		}
		row := ""
		for j := i; j < end; j++ {
			row += fmt.Sprintf("%2d.%-10s ", j+1, words[j])
		}
		fmt.Printf("  %s\n", strings.TrimRight(row, " "))
	}

	fmt.Println()
	fmt.Println("  Write these words down and store them safely.")
	fmt.Println("  Recover with: blocknet --recover")

	return nil
}

func (c *CLI) cmdImport() error {
	fmt.Printf("\n%s\n", c.sectionHead("Import"))
	fmt.Println("  1) 12-word recovery seed")
	fmt.Println("  2) spend-key/view-key (hex private keys)")
	fmt.Print("\n  Choose [1/2]: ")

	choiceLine, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read import type: %w", err)
	}
	choice := strings.ToLower(strings.TrimSpace(choiceLine))

	switch choice {
	case "1", "seed", "mnemonic", "12":
		return c.cmdImportFromMnemonic()
	case "2", "keys", "key", "spend", "view":
		return c.cmdImportFromKeys()
	default:
		return fmt.Errorf("invalid import type: choose 1 or 2")
	}
}

func (c *CLI) cmdImportFromMnemonic() error {
	fmt.Println("  Input the 12 words of your seed:")
	fmt.Print("\n> ")

	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read mnemonic: %w", err)
	}
	mnemonic := normalizeMnemonicInput(line)
	if !wallet.ValidateMnemonic(mnemonic) {
		return fmt.Errorf("invalid mnemonic phrase (expected 12 valid BIP39 words)")
	}

	base, walletPath, err := c.importWalletTargetPath()
	if err != nil {
		return err
	}

	password := c.wallet.EncryptionPasswordClone()
	defer wipeBytes(password)
	if len(password) == 0 {
		return fmt.Errorf("cannot import wallet: active wallet password unavailable")
	}

	if _, err := wallet.NewWalletFromMnemonic(walletPath, password, mnemonic, defaultWalletConfig()); err != nil {
		return fmt.Errorf("failed to create imported wallet: %w", err)
	}

	return printImportedWalletPath(base, walletPath)
}

func (c *CLI) cmdImportFromKeys() error {
	spendPriv, err := c.promptHex32("Input spend private key (64 hex chars): ")
	if err != nil {
		return fmt.Errorf("invalid spend private key: %w", err)
	}
	viewPriv, err := c.promptHex32("Input view private key (64 hex chars): ")
	if err != nil {
		return fmt.Errorf("invalid view private key: %w", err)
	}

	spendPub, err := ScalarToPubKey(spendPriv)
	if err != nil {
		return fmt.Errorf("failed to derive spend public key: %w", err)
	}
	viewPub, err := ScalarToPubKey(viewPriv)
	if err != nil {
		return fmt.Errorf("failed to derive view public key: %w", err)
	}

	base, walletPath, err := c.importWalletTargetPath()
	if err != nil {
		return err
	}

	password := c.wallet.EncryptionPasswordClone()
	defer wipeBytes(password)
	if len(password) == 0 {
		return fmt.Errorf("cannot import wallet: active wallet password unavailable")
	}

	keys := wallet.StealthKeys{
		SpendPrivKey: spendPriv,
		SpendPubKey:  spendPub,
		ViewPrivKey:  viewPriv,
		ViewPubKey:   viewPub,
	}
	if _, err := wallet.NewWalletFromStealthKeys(walletPath, password, keys, defaultWalletConfig()); err != nil {
		return fmt.Errorf("failed to create imported wallet: %w", err)
	}

	return printImportedWalletPath(base, walletPath)
}

func (c *CLI) importWalletTargetPath() (base string, walletPath string, err error) {
	fmt.Println("  Input the name of this wallet:")
	fmt.Print("\n> ")
	nameLine, err := c.reader.ReadString('\n')
	if err != nil {
		return "", "", fmt.Errorf("failed to read wallet name: %w", err)
	}
	base = filepath.Base(strings.TrimSpace(nameLine))
	if base == "" || base == "." || base == "/" {
		return "", "", fmt.Errorf("invalid wallet name")
	}
	if !strings.HasSuffix(base, ".wallet.dat") {
		base += ".wallet.dat"
	}

	walletPath = filepath.Join(filepath.Dir(c.walletFile), base)
	if _, err := os.Stat(walletPath); err == nil {
		return "", "", fmt.Errorf("wallet file already exists: %s", base)
	}
	return base, walletPath, nil
}
func (c *CLI) promptHex32(prompt string) ([32]byte, error) {
	var out [32]byte
	fmt.Print("  " + prompt)
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return out, err
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(line))
	if err != nil {
		return out, err
	}
	if len(decoded) != 32 {
		return out, fmt.Errorf("expected 64 hex chars")
	}
	copy(out[:], decoded)
	return out, nil
}

func printImportedWalletPath(base, walletPath string) error {
	resolvedPath, err := filepath.Abs(walletPath)
	if err != nil {
		resolvedPath = walletPath
	}

	fmt.Printf("\n  name: %s\n", base)
	fmt.Printf("  path: %s\n", resolvedPath)
	return nil
}

func normalizeMnemonicInput(input string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(input), ",", " ")
	return strings.Join(strings.Fields(normalized), " ")
}

func (c *CLI) viewWalletFilename() string {
	name := c.walletFile
	if strings.HasSuffix(name, ".wallet.dat") {
		return strings.TrimSuffix(name, ".wallet.dat") + ".view.wallet.dat"
	}
	ext := ""
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		ext = name[dot:]
		name = name[:dot]
	}
	return name + ".view" + ext
}

func (c *CLI) cmdViewKeys() error {
	if c.wallet.IsViewOnly() {
		return fmt.Errorf("this is already a view-only wallet")
	}

	viewFile := c.viewWalletFilename()

	fmt.Printf("\n%s\n", c.sectionHead("View Keys"))
	fmt.Println("  This will create a view-only wallet that can monitor incoming")
	fmt.Println("  funds but CANNOT spend. Anyone with this file can see your")
	fmt.Println("  balance and transaction history.")
	fmt.Printf("\n  File: %s\n", viewFile)

	if fileExists(viewFile) {
		fmt.Print("\n  File already exists. Overwrite? [y/N]: ")
	} else {
		fmt.Print("\n  Create view-only wallet? [y/N]: ")
	}

	confirm, _ := c.reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		return nil
	}

	password, err := c.promptNewPassword()
	if err != nil {
		return err
	}
	defer wipeBytes(password)

	keys := c.wallet.ExportViewOnlyKeys()
	_, err = wallet.NewViewOnlyWallet(viewFile, password, keys, defaultWalletConfig())
	if err != nil {
		return fmt.Errorf("failed to create view-only wallet: %w", err)
	}

	fmt.Printf("\n  View-only wallet saved to %s\n", viewFile)
	fmt.Println("  Load it with:")
	fmt.Printf("    blocknet --wallet %s\n", viewFile)

	return nil
}

func (c *CLI) cmdProve(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: prove <txid>")
	}

	txHashStr := sanitizeInput(args[0])
	if len(txHashStr) != 64 {
		return fmt.Errorf("txid must be 64 hex characters")
	}

	tx, _, found := c.daemon.Chain().FindTxByHashStr(txHashStr)
	if !found {
		return fmt.Errorf("transaction not found on chain")
	}

	if len(tx.Inputs) == 0 {
		return fmt.Errorf("coinbase transactions have no sender proof")
	}

	keyImages := make([][32]byte, len(tx.Inputs))
	for i, inp := range tx.Inputs {
		keyImages[i] = inp.KeyImage
	}

	keys := c.wallet.Keys()
	txPriv, err := DeriveDeterministicTxKey(keys.ViewPrivKey, keyImages)
	if err != nil {
		return fmt.Errorf("failed to derive tx key: %w", err)
	}

	derivedPub, err := ScalarToPubKey(txPriv)
	if err != nil {
		return fmt.Errorf("failed to derive public key: %w", err)
	}

	if derivedPub != tx.TxPublicKey {
		return fmt.Errorf("tx private key does not match — this transaction was not sent by this wallet (or was sent before deterministic tx keys)")
	}

	fmt.Printf("\n%s\n", c.sectionHead("Prove Payment"))
	fmt.Printf("  Tx:          %s\n", txHashStr)
	fmt.Printf("  Tx Key:      %x\n", txPriv)
	fmt.Printf("  Explorer:    https://explorer.blocknetcrypto.com/tx/%s\n", txHashStr)

	return nil
}

func (c *CLI) cmdLock() {
	c.locked = true
	fmt.Printf("\n%s\n", c.sectionHead("Locked"))
}

func (c *CLI) cmdUnlock() error {
	if !c.locked {
		fmt.Printf("\n%s\n", c.sectionHead("Unlocked"))
		fmt.Println("  Already unlocked")
		return nil
	}

	password, err := c.promptPassword("Password: ")
	if err != nil {
		return err
	}

	// Verify password matches
	if !c.passwordHashSet {
		wipeBytes(password)
		return fmt.Errorf("unlock unavailable: password state not initialized")
	}
	hash := passwordHash(password)
	wipeBytes(password)
	if subtle.ConstantTimeCompare(hash[:], c.passwordHash[:]) != 1 {
		return fmt.Errorf("incorrect password")
	}

	c.locked = false
	fmt.Printf("\n%s\n", c.sectionHead("Unlocked"))
	return nil
}

func (c *CLI) cmdAuditKeyImages() {
	outputs := c.wallet.AllOutputs()
	if len(outputs) == 0 {
		fmt.Printf("\n%s\n", c.sectionHead("Audit"))
		fmt.Println("  No outputs in wallet.")
		return
	}

	type keyImageGroup struct {
		keyImage [32]byte
		outputs  []*wallet.OwnedOutput
	}

	groups := make(map[[32]byte]*keyImageGroup)
	var failedCount int

	for _, out := range outputs {
		ki, err := GenerateKeyImage(out.OneTimePrivKey)
		if err != nil {
			failedCount++
			continue
		}
		g, ok := groups[ki]
		if !ok {
			g = &keyImageGroup{keyImage: ki}
			groups[ki] = g
		}
		g.outputs = append(g.outputs, out)
	}

	var duplicateGroups []*keyImageGroup
	for _, g := range groups {
		if len(g.outputs) > 1 {
			duplicateGroups = append(duplicateGroups, g)
		}
	}

	fmt.Printf("\n%s\n", c.sectionHead("Audit"))
	fmt.Printf("  Total outputs:      %d\n", len(outputs))
	fmt.Printf("  Unique key images:  %d\n", len(groups))
	if failedCount > 0 {
		fmt.Printf("  Failed key images:  %d\n", failedCount)
	}

	if len(duplicateGroups) == 0 {
		fmt.Println("  Duplicate key images: none")
		fmt.Println("\n  No burned funds detected.")
		return
	}

	var totalBurned uint64
	var burnedOutputs int

	fmt.Printf("  Duplicate groups:   %d\n\n", len(duplicateGroups))

	for i, g := range duplicateGroups {
		fmt.Printf("  Group %d — key image %x\n", i+1, g.keyImage)

		var groupTotal uint64
		var spentCount int
		for _, out := range g.outputs {
			groupTotal += out.Amount
			if out.Spent {
				spentCount++
			}
		}

		// Only one output per group can ever be spent. The rest are burned.
		// The "saved" amount is the largest single output in the group (best case).
		var maxAmount uint64
		for _, out := range g.outputs {
			if out.Amount > maxAmount {
				maxAmount = out.Amount
			}
		}
		burned := groupTotal - maxAmount
		totalBurned += burned
		burnedOutputs += len(g.outputs) - 1

		for _, out := range g.outputs {
			status := "unspent"
			if out.Spent {
				status = "spent"
			}
			fmt.Printf("    %x:%d  %s  %s  block %d\n",
				out.TxID, out.OutputIndex, formatAmount(out.Amount), status, out.BlockHeight)
		}
		fmt.Printf("    burned: %s (%d of %d outputs unspendable)\n\n",
			formatAmount(burned), len(g.outputs)-1, len(g.outputs))
	}

	red := "\033[38;2;255;68;68m"
	reset := "\033[0m"
	if c.noColor {
		red = ""
		reset = ""
	}
	fmt.Printf("  %sTotal burned: %s across %d duplicate outputs%s\n",
		red, formatAmount(totalBurned), burnedOutputs, reset)
	fmt.Println()
	fmt.Println("  These outputs share a key image due to a key derivation bug")
	fmt.Println("  in self-send transactions. Only one output per group can ever")
	fmt.Println("  be spent; the others are permanently unspendable.")
}

func (c *CLI) cmdSave() error {
	if err := c.wallet.Save(); err != nil {
		return fmt.Errorf("failed to save wallet: %w", err)
	}
	fmt.Printf("\n%s\n", c.sectionHead("Saved"))
	return nil
}
