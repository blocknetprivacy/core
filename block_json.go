package main

import (
	"encoding/base64"
	"slices"
	"strconv"
)

var byteDecimal = func() [256][4]byte {
	var t [256][4]byte
	for i := range t {
		s := strconv.Itoa(i)
		t[i][0] = byte(len(s))
		copy(t[i][1:], s)
	}
	return t
}()

func appendByteArrayJSON(dst []byte, b []byte) []byte {
	n := len(dst)
	dst = slices.Grow(dst, 2+4*len(b))[:n+2+4*len(b)]
	dst[n] = '['
	n++
	for i, v := range b {
		if i > 0 {
			dst[n] = ','
			n++
		}
		d := &byteDecimal[v]
		switch d[0] {
		case 1:
			dst[n] = d[1]
		case 2:
			dst[n], dst[n+1] = d[1], d[2]
		default:
			dst[n], dst[n+1], dst[n+2] = d[1], d[2], d[3]
		}
		n += int(d[0])
	}
	dst[n] = ']'
	return dst[:n+1]
}

func appendBytesJSON(dst []byte, b []byte) []byte {
	if b == nil {
		return append(dst, "null"...)
	}
	dst = append(dst, '"')
	dst = base64.StdEncoding.AppendEncode(dst, b)
	return append(dst, '"')
}

func appendHashListJSON(dst []byte, list [][32]byte) []byte {
	if list == nil {
		return append(dst, "null"...)
	}
	dst = append(dst, '[')
	for i := range list {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendByteArrayJSON(dst, list[i][:])
	}
	return append(dst, ']')
}

func appendBlockHeaderJSON(dst []byte, h *BlockHeader) []byte {
	dst = append(dst, `{"Version":`...)
	dst = strconv.AppendUint(dst, uint64(h.Version), 10)
	dst = append(dst, `,"Height":`...)
	dst = strconv.AppendUint(dst, h.Height, 10)
	dst = append(dst, `,"PrevHash":`...)
	dst = appendByteArrayJSON(dst, h.PrevHash[:])
	dst = append(dst, `,"MerkleRoot":`...)
	dst = appendByteArrayJSON(dst, h.MerkleRoot[:])
	dst = append(dst, `,"Timestamp":`...)
	dst = strconv.AppendInt(dst, h.Timestamp, 10)
	dst = append(dst, `,"Difficulty":`...)
	dst = strconv.AppendUint(dst, h.Difficulty, 10)
	dst = append(dst, `,"Nonce":`...)
	dst = strconv.AppendUint(dst, h.Nonce, 10)
	return append(dst, '}')
}

func appendTxInputJSON(dst []byte, in *TxInput) []byte {
	dst = append(dst, `{"key_image":`...)
	dst = appendByteArrayJSON(dst, in.KeyImage[:])
	dst = append(dst, `,"ring_members":`...)
	dst = appendHashListJSON(dst, in.RingMembers)
	dst = append(dst, `,"ring_commitments":`...)
	dst = appendHashListJSON(dst, in.RingCommitments)
	dst = append(dst, `,"pseudo_output":`...)
	dst = appendByteArrayJSON(dst, in.PseudoOutput[:])
	dst = append(dst, `,"ring_signature":`...)
	dst = appendBytesJSON(dst, in.RingSignature)
	return append(dst, '}')
}

func appendTxOutputJSON(dst []byte, out *TxOutput) []byte {
	dst = append(dst, `{"commitment":`...)
	dst = appendByteArrayJSON(dst, out.Commitment[:])
	dst = append(dst, `,"public_key":`...)
	dst = appendByteArrayJSON(dst, out.PublicKey[:])
	if len(out.RangeProof) > 0 {
		dst = append(dst, `,"range_proof":`...)
		dst = appendBytesJSON(dst, out.RangeProof)
	}
	dst = append(dst, `,"encrypted_amount":`...)
	dst = appendByteArrayJSON(dst, out.EncryptedAmount[:])
	dst = append(dst, `,"encrypted_memo":`...)
	dst = appendByteArrayJSON(dst, out.EncryptedMemo[:])
	return append(dst, '}')
}

func appendTransactionJSON(dst []byte, tx *Transaction) []byte {
	if tx == nil {
		return append(dst, "null"...)
	}
	dst = append(dst, `{"version":`...)
	dst = strconv.AppendUint(dst, uint64(tx.Version), 10)
	dst = append(dst, `,"inputs":`...)
	if tx.Inputs == nil {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i := range tx.Inputs {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendTxInputJSON(dst, &tx.Inputs[i])
		}
		dst = append(dst, ']')
	}
	dst = append(dst, `,"outputs":`...)
	if tx.Outputs == nil {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i := range tx.Outputs {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendTxOutputJSON(dst, &tx.Outputs[i])
		}
		dst = append(dst, ']')
	}
	dst = append(dst, `,"fee":`...)
	dst = strconv.AppendUint(dst, tx.Fee, 10)
	dst = append(dst, `,"tx_public_key":`...)
	dst = appendByteArrayJSON(dst, tx.TxPublicKey[:])
	return append(dst, '}')
}

func marshalBlockJSON(b *Block) []byte {
	if b == nil {
		return []byte("null")
	}
	size := blockHeaderSerializedSize
	for _, tx := range b.Transactions {
		if tx != nil {
			size += tx.Size()
		}
	}
	dst := make([]byte, 0, 4*size+512)
	dst = append(dst, `{"header":`...)
	dst = appendBlockHeaderJSON(dst, &b.Header)
	dst = append(dst, `,"transactions":`...)
	if b.Transactions == nil {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i, tx := range b.Transactions {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendTransactionJSON(dst, tx)
		}
		dst = append(dst, ']')
	}
	return append(dst, '}')
}
