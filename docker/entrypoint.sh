#!/bin/bash
set -e

# Blocknet Node Entrypoint Script
# Handles wallet initialization and auto-mining

echo "=========================================="
echo "  Blocknet Node Starting"
echo "=========================================="

# Build command arguments
ARGS="--daemon"
ARGS="$ARGS --data ${BLOCKNET_DATA_DIR}"
ARGS="$ARGS --wallet ${BLOCKNET_WALLET_FILE}"
ARGS="$ARGS --listen ${BLOCKNET_LISTEN}"

# Add API if configured
if [ -n "$BLOCKNET_API_ADDR" ]; then
    ARGS="$ARGS --api ${BLOCKNET_API_ADDR}"
    echo "API server: ${BLOCKNET_API_ADDR}"
fi

# Add explorer if configured
if [ -n "$BLOCKNET_EXPLORER_ADDR" ]; then
    ARGS="$ARGS --explorer ${BLOCKNET_EXPLORER_ADDR}"
    echo "Block explorer: ${BLOCKNET_EXPLORER_ADDR}"
fi

# Testnet mode
if [ "$BLOCKNET_TESTNET" = "true" ]; then
    ARGS="$ARGS --testnet"
    echo "Running in TESTNET mode"
fi

# Seed mode for bootstrap nodes
if [ "$BLOCKNET_SEED_MODE" = "true" ]; then
    ARGS="$ARGS --seed"
    echo "Running as seed node (persistent identity)"
fi

echo "Data directory: ${BLOCKNET_DATA_DIR}"
echo "Wallet file: ${BLOCKNET_WALLET_FILE}"
echo "P2P listen: ${BLOCKNET_LISTEN}"
echo "=========================================="


# Function to handle wallet password
# The daemon prompts for password on stdin
# For new wallets: needs password twice (enter + confirm)
# For existing wallets: needs password once
run_daemon() {
    if [ -z "$BLOCKNET_WALLET_PASSWORD" ]; then
        echo "Error: BLOCKNET_WALLET_PASSWORD not set"
        echo "This is required for wallet encryption"
        exit 1
    fi

    if [ "${BLOCKNET_ALLOW_WEAK_WALLET_PASSWORD}" != "true" ]; then
        if [ "$BLOCKNET_WALLET_PASSWORD" = "changeme" ]; then
            echo "Error: weak wallet password 'changeme' is not allowed by default"
            echo "Set a strong BLOCKNET_WALLET_PASSWORD or set BLOCKNET_ALLOW_WEAK_WALLET_PASSWORD=true to opt in (unsafe)"
            exit 1
        fi
        if [ "${#BLOCKNET_WALLET_PASSWORD}" -lt 12 ]; then
            echo "Error: BLOCKNET_WALLET_PASSWORD must be at least 12 characters by default"
            echo "Set BLOCKNET_ALLOW_WEAK_WALLET_PASSWORD=true to opt in (unsafe)"
            exit 1
        fi
    fi

    if [ -f "$BLOCKNET_WALLET_FILE" ]; then
        # Existing wallet - send password once
        echo "Opening existing wallet..."
        echo "$BLOCKNET_WALLET_PASSWORD" | /app/blocknet $ARGS
    else
        # New wallet - send password twice (enter + confirm)
        echo "Creating new wallet..."
        printf '%s\n%s\n' "$BLOCKNET_WALLET_PASSWORD" "$BLOCKNET_WALLET_PASSWORD" | /app/blocknet $ARGS
    fi
}

# Mining is intentionally NOT started automatically.
# Use the API to start mining if desired (see docker/README.md).

# Run the daemon (this blocks)
run_daemon
