package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// MaxMessageSize is the maximum size of a single message (16 MB)
	MaxMessageSize = 16 * 1024 * 1024

	// Stream-level payload hard caps. New blocks are announced as JSON, which
	// writes each byte array as decimal numbers, so a block at the 1 MiB
	// consensus limit can encode to about 4 MiB.
	MaxBlockStreamPayloadSize     = 6 * 1024 * 1024
	MaxTxStreamPayloadSize        = 1 * 1024 * 1024
	MaxDandelionStreamPayloadSize = MaxTxStreamPayloadSize

	// Typed protocol payload hard caps.
	MaxPEXMessageSize          = 512 * 1024
	MaxSyncStatusMessageSize   = 32 * 1024
	MaxSyncHeadersMessageSize  = 3 * 1024 * 1024
	MaxSyncBlocksMessageSize   = 12 * 1024 * 1024
	MaxSyncMempoolMessageSize  = 6 * 1024 * 1024
	MaxSyncGetHeadersReqSize   = 32 * 1024
	MaxSyncGetBlocksReqSize    = 64 * 1024
	MaxSyncGetBlocksByHeightSz = 32 * 1024
	MaxSyncGetMempoolReqSize   = 4 * 1024

	// MaxSyncMempoolTxCount caps decoded transaction entries in a mempool
	// sync response. Aligned with the default mempool capacity (5,000).
	MaxSyncMempoolTxCount = 5000
)

// writeLengthPrefixed writes data with a 4-byte big-endian length prefix
func writeLengthPrefixed(w io.Writer, data []byte) error {
	if len(data) > MaxMessageSize {
		return fmt.Errorf("message too large: %d > %d", len(data), MaxMessageSize)
	}

	// Write length prefix
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := w.Write(lenBuf); err != nil {
		return err
	}

	// Write data
	_, err := w.Write(data)
	return err
}

// readLengthPrefixedWithLimit reads length-prefixed data with an explicit cap.
func readLengthPrefixedWithLimit(r io.Reader, maxSize uint32) ([]byte, error) {
	// Read length prefix
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > maxSize {
		return nil, fmt.Errorf("message too large: %d > %d", length, maxSize)
	}

	// Read data
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return data, nil
}

// writeMessage writes a message type byte followed by length-prefixed data
func writeMessage(w io.Writer, msgType byte, data []byte) error {
	if _, err := w.Write([]byte{msgType}); err != nil {
		return err
	}
	return writeLengthPrefixed(w, data)
}

// readMessageWithLimit reads a type byte then a length-prefixed payload using
// a message-type-specific payload cap.
func readMessageWithLimit(r io.Reader, maxForType func(byte) (uint32, error)) (byte, []byte, error) {
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, typeBuf); err != nil {
		return 0, nil, err
	}

	maxSize, err := maxForType(typeBuf[0])
	if err != nil {
		return 0, nil, err
	}

	data, err := readLengthPrefixedWithLimit(r, maxSize)
	if err != nil {
		return 0, nil, err
	}

	return typeBuf[0], data, nil
}

// isExpectedStreamCloseError returns true for close/reset errors that are common
// when the remote peer already hung up (disconnects, restarts, conn manager, etc).
// These are noisy and not actionable for normal operators, so callers can suppress
// console logging for them.
func isExpectedStreamCloseError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}

	// libp2p often wraps these as plain errors with descriptive text.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "stream reset"),
		strings.Contains(msg, "connection closed"),
		strings.Contains(msg, "use of closed network connection"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "reset by peer"):
		return true
	default:
		return false
	}
}
