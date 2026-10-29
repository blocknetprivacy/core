package main

import (
	"testing"

	"blocknet/p2p"
)

// A block at the consensus size limit, filled with bytes that take the most
// JSON digits, must still fit a new-block announcement.
func TestFullSizeBlockFitsTheAnnouncementLimit(t *testing.T) {
	block := &Block{Header: BlockHeader{Version: 1, Height: 1 << 40, Timestamp: 1 << 40, Difficulty: 1 << 60, Nonce: 1 << 63}}
	size := blockHeaderSerializedSize
	for size < MaxBlockSize {
		tx := &Transaction{Version: 1, Fee: 1 << 60}
		for i := range tx.TxPublicKey {
			tx.TxPublicKey[i] = 255
		}
		var out TxOutput
		for i := range out.Commitment {
			out.Commitment[i] = 255
			out.PublicKey[i] = 255
		}
		for i := range out.EncryptedAmount {
			out.EncryptedAmount[i] = 255
		}
		for i := range out.EncryptedMemo {
			out.EncryptedMemo[i] = 255
		}
		for j := 0; j < 16; j++ {
			tx.Outputs = append(tx.Outputs, out)
		}
		block.Transactions = append(block.Transactions, tx)
		size += tx.Size()
	}
	encoded := len(marshalBlockJSON(block))
	if encoded > p2p.MaxBlockStreamPayloadSize {
		t.Fatalf("a %d-byte block encodes to %d bytes of JSON, over the %d-byte announcement limit", size, encoded, p2p.MaxBlockStreamPayloadSize)
	}
	t.Logf("%d-byte block encodes to %d bytes of JSON", size, encoded)
}
