package main

import (
	"fmt"
	"strings"
)

type helpEntry struct {
	usage         []string
	aliases       []string
	description   []string
	useWhen       []string
	exampleInput  []string
	exampleOutput []string
	notes         []string
}

func normalizeHelpTopic(topic string) string {
	switch strings.ToLower(topic) {
	case "help", "?":
		return "help"
	case "balance", "bal", "b":
		return "balance"
	case "address", "addr", "a":
		return "address"
	case "send":
		return "send"
	case "sign":
		return "sign"
	case "verify":
		return "verify"
	case "history", "hist", "h":
		return "history"
	case "outputs", "outs", "out":
		return "outputs"
	case "seed":
		return "seed"
	case "import":
		return "import"
	case "viewkeys":
		return "viewkeys"
	case "lock":
		return "lock"
	case "unlock":
		return "unlock"
	case "save":
		return "save"
	case "sync", "scan":
		return "sync"
	case "status":
		return "status"
	case "peers":
		return "peers"
	case "banned":
		return "banned"
	case "export-peer":
		return "export-peer"
	case "mining":
		return "mining"
	case "certify":
		return "certify"
	case "save-checkpoints":
		return "save-checkpoints"
	case "load-checkpoints":
		return "load-checkpoints"
	case "purge":
		return "purge"
	case "version":
		return "version"
	case "about":
		return "about"
	case "license":
		return "license"
	case "quit", "exit", "q":
		return "quit"
	default:
		return ""
	}
}

func helpCommandDetails(viewOnly bool, noColor bool) map[string]helpEntry {
	blocknetIDURL := "https://blocknet.id"
	historyIn := "IN"
	historyOut := "OUT"
	outUnspent := "unspent"
	outSpent := "spent"
	outPending := "pending"
	if !noColor {
		blocknetIDURL = "\033[4m\033[38;2;170;255;0mhttps://blocknet.id\033[0m"
		historyIn = "\033[38;2;170;255;0mIN\033[0m"
		historyOut = "\033[38;2;255;68;68mOUT\033[0m"
		outUnspent = "\033[38;2;170;255;0munspent\033[0m"
		outSpent = "\033[38;2;255;68;68mspent\033[0m"
		outPending = "\033[38;2;255;170;0mpending\033[0m"
	}

	sendNotes := []string{
		"you can send whole numbers or fractions (example: 1 or 1.25 BNT)",
		"memos with spaces are supported",
		"memo has a size limit; keep it short (about a short sentence)",
		"'verified' means the @name or $name address proof checked out and was not tampered with",
		"short names can be used as @name or $name",
	}
	if viewOnly {
		sendNotes = append(sendNotes, "view-only wallets cannot send")
	}

	return map[string]helpEntry{
		"help": {
			usage:         []string{"help", "help <command>"},
			aliases:       []string{"?"},
			description:   []string{"Shows all commands or detailed help for one command."},
			useWhen:       []string{"you are not sure what command to run next"},
			exampleInput:  []string{"> help send"},
			exampleOutput: []string{"# Help — send", "  What it does:", "    Sends BNT to another wallet, optionally with a note."},
		},
		"balance": {
			usage:         []string{"balance"},
			aliases:       []string{"bal", "b"},
			description:   []string{"Shows your spendable coins, pending coins, and total."},
			useWhen:       []string{"you want to know how much you can spend right now"},
			exampleInput:  []string{"> bal"},
			exampleOutput: []string{"# Balance", "  spendable:  12.5 BNT", "  confirming: 1 BNT", "  total:      13.5 BNT"},
		},
		"address": {
			usage:         []string{"address"},
			aliases:       []string{"addr", "a"},
			description:   []string{"Shows your receive address to share with someone paying you."},
			useWhen:       []string{"someone asks where to send you coins"},
			exampleInput:  []string{"> addr"},
			exampleOutput: []string{"# Address", "  bnt1q..."},
			notes:         []string{"Get a short name like @name or $name at " + blocknetIDURL},
		},
		"send": {
			usage:       []string{"send <addr> <amt> [memo|hex:<memo_hex>]"},
			description: []string{"Sends BNT to another wallet, optionally with a note."},
			useWhen:     []string{"you need to pay someone now"},
			exampleInput: []string{
				"> send @rock 100 \"hello\"",
				"> send $rock 100",
				"> send 5XQv7kXyf5SN7eraZ3UUNgLqmMBQpxXWEC5mHiQndiiCeHZagQNqRdFVihTdiXu7bjdG7ZQ51YrZqDBgKJMpxftDjBD9B 1.25",
			},
			exampleOutput: []string{
				"Resolved $rock -> bnt1qxy... ✓ verified",
				"Sent: 9f0b...",
				"Explorer: https://explorer.blocknetcrypto.com/tx/9f0b...",
			},
			notes: sendNotes,
		},
		"sign": {
			usage:         []string{"sign"},
			description:   []string{"Signs a message so you can prove wallet ownership."},
			useWhen:       []string{"a service asks you to prove this wallet is yours"},
			exampleInput:  []string{"> sign", "  (then type: prove wallet ownership)"},
			exampleOutput: []string{"# Sign", "  8f2d... (signature hex)"},
			notes:         []string{"watch-only wallets cannot sign", "message should be short (up to about 1,000 characters)"},
		},
		"verify": {
			usage:         []string{"verify"},
			description:   []string{"Checks if a signature really came from an address."},
			useWhen:       []string{"you received a signed message and need to trust it"},
			exampleInput:  []string{"> verify", "  (then enter address, message, signature)"},
			exampleOutput: []string{"# Signature is VALID"},
			notes:         []string{"signature must be pasted exactly as produced by sign"},
		},
		"history": {
			usage:         []string{"history"},
			aliases:       []string{"hist", "h"},
			description:   []string{"Shows money in and money out, oldest to newest."},
			useWhen:       []string{"you need to review recent wallet activity"},
			exampleInput:  []string{"> hist"},
			exampleOutput: []string{
				"# History",
				fmt.Sprintf("  260224-09:11:00 %s  2 BNT ...", historyIn),
				fmt.Sprintf("  260224-11:42:10 %s 1.2 BNT ...", historyOut),
			},
		},
		"outputs": {
			usage:         []string{"outputs [spent|unspent|pending] [index]", "outputs tx <txid>", "outputs tx <txid>:<index>"},
			aliases:       []string{"outs", "out"},
			description:   []string{"Shows outputs your wallet owns, with status and drill-down details."},
			useWhen:       []string{"you want to inspect spendable/spent/pending outputs or one specific output"},
			exampleInput: []string{
				"> outputs",
				"> outputs unspent",
				"> outputs pending 2",
				"> outputs tx a1b2c3...d4e5",
				"> outputs tx a1b2c3...d4e5:0",
			},
			exampleOutput: []string{
				"# Outputs",
				fmt.Sprintf("  #1  %s      regular   conf: 25", outUnspent),
				"      amount: 2.5 BNT",
				fmt.Sprintf("  #2  %s        regular   conf: 4", outPending),
				fmt.Sprintf("  #3  %s          coinbase  conf: 120", outSpent),
				"      block: 1240  tx: a1b2...:0",
			},
			notes: []string{
				"use filters: spent, unspent, pending",
				"use an index to open one output detail page (example: outputs 3)",
				"`outputs tx <txid>` shows all owned outputs in that tx",
				"`outputs tx <txid>:<index>` shows one exact output",
				"if scan is still catching up, run sync and check again",
			},
		},
		"seed": {
			usage:         []string{"seed"},
			description:   []string{"Shows your 12-word recovery seed after warning prompts."},
			useWhen:       []string{"you are backing up wallet recovery words"},
			exampleInput:  []string{"> seed"},
			exampleOutput: []string{"# Seed", "  WARNING: Your recovery seed controls all funds.", "  Show recovery seed? [y/N]:"},
			notes:         []string{"anyone with this seed can spend your funds"},
		},
		"import": {
			usage:         []string{"import"},
			description:   []string{"Creates a new wallet file from a seed phrase or secret key text."},
			useWhen:       []string{"you need to load an existing wallet into this node"},
			exampleInput:  []string{"> import", "  Choose [1/2]: 1"},
			exampleOutput: []string{"# Import", "  1) 12-word recovery seed", "  2) spend-key/view-key (private key text)"},
			notes:         []string{"creates a new wallet file; does not replace current wallet"},
		},
		"viewkeys": {
			usage:         []string{"viewkeys"},
			description:   []string{"Creates a wallet file that can watch funds but cannot spend."},
			useWhen:       []string{"you want watch-only access on another machine"},
			exampleInput:  []string{"> viewkeys"},
			exampleOutput: []string{"# View Keys", "  This will create a view-only wallet...", "  View-only wallet saved to my.view.wallet.dat"},
			notes:         []string{"view-only wallets can monitor but cannot spend"},
		},
		"lock": {
			usage:         []string{"lock"},
			description:   []string{"Locks wallet actions that require your password."},
			useWhen:       []string{"you are stepping away from your terminal"},
			exampleInput:  []string{"> lock"},
			exampleOutput: []string{"# Locked"},
		},
		"unlock": {
			usage:         []string{"unlock"},
			description:   []string{"Unlocks wallet actions after password confirmation."},
			useWhen:       []string{"you get a 'wallet is locked' error"},
			exampleInput:  []string{"> unlock"},
			exampleOutput: []string{"Password: ", "# Unlocked"},
		},
		"save": {
			usage:         []string{"save"},
			description:   []string{"Forces an immediate wallet save to disk."},
			useWhen:       []string{"you want to persist changes right now"},
			exampleInput:  []string{"> save"},
			exampleOutput: []string{"# Saved"},
		},
		"sync": {
			usage:         []string{"sync"},
			aliases:       []string{"scan"},
			description:   []string{"Scans blockchain blocks so wallet balance/history catches up."},
			useWhen:       []string{"wallet looks behind chain height or missing transactions"},
			exampleInput:  []string{"> sync"},
			exampleOutput: []string{"# Sync", "  Known blocks:   2450", "  Blocks scanned: 2400", "  Scanning 50 blocks..."},
		},
		"status": {
			usage:         []string{"status"},
			description:   []string{"Shows node health and wallet summary in one screen."},
			useWhen:       []string{"you need a quick 'is everything healthy?' check"},
			exampleInput:  []string{"> status"},
			exampleOutput: []string{"# Node", "  Peers: 8", "  Height: 2450", "# Wallet", "  Balance: 13.5 BNT"},
		},
		"peers": {
			usage:         []string{"peers"},
			description:   []string{"Lists currently connected peers."},
			useWhen:       []string{"you need to confirm network connectivity"},
			exampleInput:  []string{"> peers"},
			exampleOutput: []string{"# Peers (8)", "  12D3KooW..."},
		},
		"banned": {
			usage:         []string{"banned"},
			description:   []string{"Shows peers that were banned and why."},
			useWhen:       []string{"you suspect peer filtering or connectivity issues"},
			exampleInput:  []string{"> banned"},
			exampleOutput: []string{"# Banned (1)", "  12D3KooW...", "    reason: repeated bad blocks"},
		},
		"export-peer": {
			usage:         []string{"export-peer"},
			description:   []string{"Writes your node addresses to peer.txt for bootstrap sharing."},
			useWhen:       []string{"you want another node to connect to this one"},
			exampleInput:  []string{"> export-peer"},
			exampleOutput: []string{"# Export", "  Peer addresses written to peer.txt"},
		},
		"mining": {
			usage:         []string{"mining", "mining start", "mining stop", "mining threads <N>"},
			description:   []string{"Controls local mining and how many CPU threads mining uses."},
			useWhen:       []string{"you want to mine, stop mining, or tune CPU/RAM use"},
			exampleInput: []string{
				"> mining start",
				"> mining threads",
				"> mining threads 4",
				"> mining stop",
			},
			exampleOutput: []string{
				"# Mining",
				"  Started with 2 threads (~4GB RAM)",
				"# Mining",
				"  Threads: 2",
				"# Mining",
				"  Threads set to 4 (~8GB RAM)",
			},
			notes: []string{
				"roughly 2GB RAM per thread",
				"thread aliases: threads, thread, t, thrads, thrad",
			},
		},
		"certify": {
			usage:         []string{"certify"},
			description:   []string{"Checks the chain for broken or inconsistent block data."},
			useWhen:       []string{"you suspect corruption or strange chain behavior"},
			exampleInput:  []string{"> certify"},
			exampleOutput: []string{"# Certify", "  Checking 2450 blocks...", "  Chain is clean. All 2450 blocks passed."},
		},
		"save-checkpoints": {
			usage:         []string{"save-checkpoints"},
			description:   []string{"Writes checkpoint entries for every 100th block to checkpoints.dat."},
			useWhen:       []string{"you want to generate checkpoints from your verified chain"},
			exampleInput:  []string{"> save-checkpoints"},
			exampleOutput: []string{"# Save Checkpoints", "  Wrote 24 checkpoint(s) to /home/user/.blocknet/checkpoints.dat"},
			notes:         []string{"only writes new entries not already in the file", "works regardless of --save-checkpoints flag"},
		},
		"load-checkpoints": {
			usage:         []string{"load-checkpoints"},
			description:   []string{"Downloads (if missing) and loads checkpoints for faster sync."},
			useWhen:       []string{"you want to enable PoW skipping mid-sync without restarting"},
			exampleInput:  []string{"> load-checkpoints"},
			exampleOutput: []string{"# Load Checkpoints", "  Loaded 24 checkpoint(s) (max height: 2400)", "  Fast-sync enabled up to height 2400 (chain is at 1200)"},
			notes:         []string{"downloads from remote if no local file exists", "has no effect once chain is past the last checkpoint height"},
		},
		"purge": {
			usage:         []string{"purge"},
			description:   []string{"Deletes local chain data but keeps your wallet and funds."},
			useWhen:       []string{"chain is stuck/corrupted and regular sync cannot recover"},
			exampleInput:  []string{"> purge", "  Confirm purge? [y/N]: y"},
			exampleOutput: []string{"# Purge", "  Stopping daemon...", "  Blockchain data purged. Restart and it will download chain data again from block 1."},
			notes:         []string{"your wallet file and money are not deleted", "this command shuts down the node after purge"},
		},
		"version": {
			usage:         []string{"version"},
			description:   []string{"Prints the exact Blocknet version you are running."},
			useWhen:       []string{"you are reporting bugs or checking for updates"},
			exampleInput:  []string{"> version"},
			exampleOutput: []string{"# Version 1.2.3"},
		},
		"about": {
			usage:         []string{"about"},
			description:   []string{"Shows project info, links, and third-party library credits."},
			useWhen:       []string{"you want project metadata and upstream links"},
			exampleInput:  []string{"> about"},
			exampleOutput: []string{"# About", "  Blocknet v1.2.3", "  https://github.com/blocknetprivacy"},
		},
		"license": {
			usage:         []string{"license"},
			description:   []string{"Prints the full software license text."},
			useWhen:       []string{"you need legal/license details"},
			exampleInput:  []string{"> license"},
			exampleOutput: []string{"# License", "  BSD 3-Clause License ..."},
		},
		"quit": {
			usage:         []string{"quit"},
			aliases:       []string{"exit", "q"},
			description:   []string{"Saves wallet, stops daemon, and exits the app cleanly."},
			useWhen:       []string{"you are done and want a clean shutdown"},
			exampleInput:  []string{"> q"},
			exampleOutput: []string{"# Shutdown", "  Saving wallet...", "  Stopping daemon...", "  Done"},
		},
	}
}

func (c *CLI) cmdHelp(args []string) {
	if len(args) > 0 {
		topic := normalizeHelpTopic(args[0])
		if topic == "" {
			fmt.Printf("\n%s\n", c.errorHead("Help"))
			fmt.Printf("  Unknown command: %s\n", args[0])
			fmt.Println("  Use 'help' to list available commands.")
			return
		}

		entry, ok := helpCommandDetails(c.wallet.IsViewOnly(), c.noColor)[topic]
		if !ok {
			fmt.Printf("\n%s\n", c.errorHead("Help"))
			fmt.Printf("  No detailed help available for: %s\n", topic)
			return
		}

		labelColor := "\033[38;2;170;255;0m"
		resetColor := "\033[0m"
		if c.noColor {
			labelColor = ""
			resetColor = ""
		}

		fmt.Printf("\n%s\n", c.sectionHead("Help — "+topic))
		if len(entry.aliases) > 0 {
			fmt.Printf("  %sShort name%s:\n", labelColor, resetColor)
			fmt.Printf("    %s\n", strings.Join(entry.aliases, ", "))
			fmt.Println()
		}

		fmt.Printf("  %sWhat it does%s:\n", labelColor, resetColor)
		for _, line := range entry.description {
			fmt.Printf("    %s\n", line)
		}

		if len(entry.useWhen) > 0 {
			fmt.Println()
			fmt.Printf("  %sUse this when%s:\n", labelColor, resetColor)
			for _, line := range entry.useWhen {
				fmt.Printf("    %s\n", line)
			}
		}

		if len(entry.exampleInput) > 0 {
			fmt.Println()
			fmt.Printf("  %sExample input%s:\n", labelColor, resetColor)
			for _, line := range entry.exampleInput {
				fmt.Printf("    %s\n", line)
			}
		}

		if len(entry.exampleOutput) > 0 {
			fmt.Println()
			fmt.Printf("  %sexample output%s:\n", labelColor, resetColor)
			for _, line := range entry.exampleOutput {
				fmt.Printf("    %s\n", line)
			}
		}

		if len(entry.notes) > 0 {
			fmt.Println()
			fmt.Printf("  %sNotes%s:\n", labelColor, resetColor)
			for _, line := range entry.notes {
				fmt.Printf("    %s-%s %s\n", labelColor, resetColor, line)
			}
		}
		return
	}

	viewOnlyNote := ""
	if c.wallet.IsViewOnly() {
		viewOnlyNote = " [VIEW-ONLY]"
	}

	sendNote := ""
	if c.wallet.IsViewOnly() {
		sendNote = " (disabled)"
	}

	fmt.Printf(`
%s%s
  balance           Show wallet balance
  address           Show receiving address
  send <addr> <amt> [memo|hex:<memo_hex>] Send funds with optional memo%s
  sign              Sign a message with your spend key
  verify            Verify a signed message against an address
  history           Show transaction history
  outputs           Show wallet outputs (spent and unspent)
  seed              Show wallet recovery seed (careful!)
  import            Create wallet file from seed or spend/view keys
  viewkeys          Create a view-only wallet file
  lock              Lock wallet
  unlock            Unlock wallet
  save              Save wallet to disk
  sync              Rescan blocks for outputs

%s
  status            Show node and wallet status
  peers             List connected peers
  banned            List banned peers
  export-peer       Export peer address to peer.txt
  mining            Manage mining
  certify           Check chain integrity (difficulty + timestamps)
  save-checkpoints  Write checkpoints for known blocks to disk
  load-checkpoints  Download/reload checkpoints for faster sync
  purge             Delete all blockchain data (cannot be undone)
  version           Print version
  about             About this software
  license           Show license
  quit              Exit (saves automatically)
  help <command>    Show detailed help for a command

  Need more info on any command? Type: help <command>
  Example: help send
`, c.sectionHead("Wallet"), viewOnlyNote, sendNote, c.sectionHead("Daemon"))
}
