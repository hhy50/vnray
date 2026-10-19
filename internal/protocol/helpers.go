package protocol

import "encoding/binary"

// 这组小包装统一大端序的定长整数读写，避免在调用处重复切片边界。

func putUint16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }

func getUint16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }

func putUint32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

func getUint32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func putUint64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

func getUint64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
