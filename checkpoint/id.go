package checkpoint

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gregorianUnixOffset100ns uint64 = 0x01b21dd213814000

var idClock struct {
	sync.Mutex
	last uint64
}

// NewID returns a lexicographically time-ordered RFC 9562 UUIDv6. Step occupies the
// clock-sequence bits so checkpoints created at one timestamp retain execution order.
func NewID(step int) (string, error) {
	now := uint64(time.Now().UnixNano()/100) + gregorianUnixOffset100ns
	idClock.Lock()
	if now <= idClock.last {
		now = idClock.last + 1
	}
	idClock.last = now
	idClock.Unlock()

	var value [16]byte
	binary.BigEndian.PutUint32(value[0:4], uint32(now>>28))
	binary.BigEndian.PutUint16(value[4:6], uint16(now>>12))
	binary.BigEndian.PutUint16(value[6:8], uint16(now&0x0fff)|0x6000)
	clockSequence := uint16(step) & 0x3fff
	binary.BigEndian.PutUint16(value[8:10], clockSequence|0x8000)
	if _, err := rand.Read(value[10:]); err != nil {
		return "", fmt.Errorf("generate checkpoint id: %w", err)
	}
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16],
	), nil
}

// NextVersion returns the PostgreSQL-compatible lexical channel version format.
func NextVersion(current string) (string, error) {
	integer := new(big.Int)
	if current != "" {
		prefix := strings.SplitN(current, ".", 2)[0]
		if _, ok := integer.SetString(prefix, 10); !ok {
			return "", fmt.Errorf("parse channel version %q", current)
		}
	}
	integer.Add(integer, big.NewInt(1))
	prefix := integer.String()
	if len(prefix) < 32 {
		prefix = strings.Repeat("0", 32-len(prefix)) + prefix
	}

	limit := new(big.Int).Lsh(big.NewInt(1), 53)
	random, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", fmt.Errorf("generate channel version: %w", err)
	}
	fraction := float64(random.Int64()) / float64(uint64(1)<<53)
	formatted := strconv.FormatFloat(fraction, 'g', -1, 64)
	if len(formatted) < 16 {
		formatted = strings.Repeat("0", 16-len(formatted)) + formatted
	}
	return prefix + "." + formatted, nil
}
