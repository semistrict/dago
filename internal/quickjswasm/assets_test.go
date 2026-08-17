package quickjswasm

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestPinnedArtifactDigests(t *testing.T) {
	tests := []struct {
		name, want string
		data       []byte
	}{
		{name: "guest", data: Guest, want: "e8dec0ddb980454a0abc4cb54cfe6960a022efdc382a11501fd53f3836862490"},
		{name: "tracked guest", data: TrackedGuest, want: "3419fa56544d7dcebf9708a139c2f46d019f595bd675aa04a7a82b35248cac1d"},
		{name: "transform", data: Transform, want: "c4781c9556ec79e8af2295e72d56214fa977d04c907d8f50192f7b2c71a0022d"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fmt.Sprintf("%x", sha256.Sum256(test.data)); got != test.want {
				t.Fatalf("sha256 = %s, want %s", got, test.want)
			}
		})
	}
}
