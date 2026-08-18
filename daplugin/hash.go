package daplugin

import "crypto/sha256"

func sha256Bytes(value []byte) [32]byte { return sha256.Sum256(value) }
