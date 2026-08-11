package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"blocknet/protocol/params"
)

// txStorageRingCeiling bounds ring size when decoding a block already on disk.
// Consensus keeps using RingSize, so what the network accepts is unchanged; this
// only stops a future RingSize change from making a node unable to read its own
// history. Storage records were written by us and already passed validation.
const txStorageRingCeiling = 256

// DeserializeStoredTx parses a transaction out of the block store.
func DeserializeStoredTx(data []byte) (*Transaction, error) {
	return deserializeTx(data, txStorageRingCeiling)
}

// Stored blocks carry a leading format tag. Legacy values are JSON objects, so
// they always begin with '{' — a byte the binary tags never use.
const (
	blockFormatBinaryV1 byte = 0x01
	utxoFormatBinaryV1  byte = 0x01
	blockFormatJSONMark byte = '{'
)

// encodeBlock produces the stored representation of a block:
//
//	tag(1) | header(blockHeaderSerializedSize) | uvarint txCount |
//	per tx: uvarint length | canonical transaction bytes
//
// It reuses the canonical serializers, so a stored block decodes to exactly the
// bytes consensus already hashes. Storage encoding is not consensus-visible:
// Block.Hash is the header hash, and the merkle root covers the transactions.
func encodeBlock(block *Block) ([]byte, error) {
	if block == nil {
		return nil, fmt.Errorf("cannot encode nil block")
	}

	txBytes := make([][]byte, len(block.Transactions))
	total := 1 + blockHeaderSerializedSize + binary.MaxVarintLen64
	for i, tx := range block.Transactions {
		if tx == nil {
			return nil, fmt.Errorf("cannot encode nil transaction at index %d", i)
		}
		txBytes[i] = tx.Serialize()
		total += binary.MaxVarintLen64 + len(txBytes[i])
	}

	buf := make([]byte, 0, total)
	buf = append(buf, blockFormatBinaryV1)
	buf = append(buf, block.Header.Serialize()...)
	buf = binary.AppendUvarint(buf, uint64(len(txBytes)))
	for _, data := range txBytes {
		buf = binary.AppendUvarint(buf, uint64(len(data)))
		buf = append(buf, data...)
	}

	return buf, nil
}

// decodeBlockBinaryV1 parses the format written by encodeBlock.
func decodeBlockBinaryV1(data []byte) (*Block, error) {
	if len(data) < 1+blockHeaderSerializedSize {
		return nil, fmt.Errorf("stored block too short: %d bytes", len(data))
	}

	header, err := deserializeBlockHeader(data[1 : 1+blockHeaderSerializedSize])
	if err != nil {
		return nil, err
	}

	rest := data[1+blockHeaderSerializedSize:]
	txCount, n := binary.Uvarint(rest)
	if n <= 0 {
		return nil, fmt.Errorf("stored block has an unreadable transaction count")
	}
	rest = rest[n:]

	if txCount > uint64(len(rest))+1 {
		return nil, fmt.Errorf("stored block claims %d transactions in %d bytes", txCount, len(rest))
	}

	block := &Block{Header: header}
	if txCount > 0 {
		block.Transactions = make([]*Transaction, 0, txCount)
	}

	for i := uint64(0); i < txCount; i++ {
		size, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("stored block transaction %d has an unreadable length", i)
		}
		rest = rest[n:]
		if size > uint64(len(rest)) {
			return nil, fmt.Errorf("stored block transaction %d claims %d bytes, %d remain", i, size, len(rest))
		}

		tx, err := DeserializeStoredTx(rest[:size])
		if err != nil {
			return nil, fmt.Errorf("stored block transaction %d: %w", i, err)
		}
		block.Transactions = append(block.Transactions, tx)
		rest = rest[size:]
	}

	if len(rest) != 0 {
		return nil, fmt.Errorf("stored block has %d trailing bytes", len(rest))
	}

	return block, nil
}

// deserializeBlockHeader is the inverse of BlockHeader.serializeFull.
func deserializeBlockHeader(data []byte) (BlockHeader, error) {
	var header BlockHeader
	if len(data) != blockHeaderSerializedSize {
		return header, fmt.Errorf("block header is %d bytes, want %d", len(data), blockHeaderSerializedSize)
	}

	offset := 0
	header.Version = binary.LittleEndian.Uint32(data[offset:])
	offset += 4

	header.Height = binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	copy(header.PrevHash[:], data[offset:offset+32])
	offset += 32

	copy(header.MerkleRoot[:], data[offset:offset+32])
	offset += 32

	header.Timestamp = int64(binary.LittleEndian.Uint64(data[offset:]))
	offset += 8

	header.Difficulty = binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	header.Nonce = binary.LittleEndian.Uint64(data[offset:])

	return header, nil
}

// utxoBinarySize is the fixed part of a stored output: txid, output index,
// block height, then the output's fixed fields. Only the range proof varies.
const utxoBinarySize = 1 + 32 + 4 + 8 + 32 + 32 + 8 + params.MemoSize

// encodeUTXO mirrors encodeBlock: a format tag, then fixed fields in the same
// order the canonical transaction encoding uses, then a length-prefixed proof.
func encodeUTXO(output *UTXO) ([]byte, error) {
	if output == nil {
		return nil, fmt.Errorf("cannot encode nil output")
	}

	buf := make([]byte, utxoBinarySize, utxoBinarySize+binary.MaxVarintLen64+len(output.Output.RangeProof))
	off := 0

	buf[off] = utxoFormatBinaryV1
	off++

	copy(buf[off:], output.TxID[:])
	off += 32

	binary.LittleEndian.PutUint32(buf[off:], output.OutputIndex)
	off += 4

	binary.LittleEndian.PutUint64(buf[off:], output.BlockHeight)
	off += 8

	copy(buf[off:], output.Output.PublicKey[:])
	off += 32

	copy(buf[off:], output.Output.Commitment[:])
	off += 32

	copy(buf[off:], output.Output.EncryptedAmount[:])
	off += 8

	copy(buf[off:], output.Output.EncryptedMemo[:])
	off += params.MemoSize

	if off != utxoBinarySize {
		return nil, fmt.Errorf("output encoding invariant violated at %d", off)
	}

	buf = binary.AppendUvarint(buf, uint64(len(output.Output.RangeProof)))
	buf = append(buf, output.Output.RangeProof...)

	return buf, nil
}

func decodeUTXOBinaryV1(data []byte) (*UTXO, error) {
	if len(data) < utxoBinarySize {
		return nil, fmt.Errorf("stored output too short: %d bytes", len(data))
	}

	output := &UTXO{}
	off := 1

	copy(output.TxID[:], data[off:off+32])
	off += 32

	output.OutputIndex = binary.LittleEndian.Uint32(data[off:])
	off += 4

	output.BlockHeight = binary.LittleEndian.Uint64(data[off:])
	off += 8

	copy(output.Output.PublicKey[:], data[off:off+32])
	off += 32

	copy(output.Output.Commitment[:], data[off:off+32])
	off += 32

	copy(output.Output.EncryptedAmount[:], data[off:off+8])
	off += 8

	copy(output.Output.EncryptedMemo[:], data[off:off+params.MemoSize])
	off += params.MemoSize

	rest := data[off:]
	size, n := binary.Uvarint(rest)
	if n <= 0 {
		return nil, fmt.Errorf("stored output has an unreadable range proof length")
	}
	rest = rest[n:]
	if size != uint64(len(rest)) {
		return nil, fmt.Errorf("stored output range proof claims %d bytes, %d remain", size, len(rest))
	}
	if size > 0 {
		output.Output.RangeProof = make([]byte, size)
		copy(output.Output.RangeProof, rest)
	}

	return output, nil
}

// decodeUTXO reads either the current binary format or the legacy JSON one.
func decodeUTXO(data []byte) (*UTXO, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("cannot decode empty output record")
	}

	switch data[0] {
	case utxoFormatBinaryV1:
		return decodeUTXOBinaryV1(data)
	case blockFormatJSONMark:
		output := &UTXO{}
		if err := json.Unmarshal(data, output); err != nil {
			return nil, err
		}
		return output, nil
	default:
		return nil, fmt.Errorf("unknown stored output format tag 0x%02x", data[0])
	}
}
