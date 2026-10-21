package mining

import "testing"

func TestCountStartZeroBits(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want int
	}{
		{[]byte{0x80}, 0},
		{[]byte{0x40}, 1},
		{[]byte{0x01}, 7},
		{[]byte{0x00, 0x01}, 15},
		{[]byte{0x00, 0x00}, 16},
	} {
		if got := CountStartZeroBits(tc.in); got != tc.want {
			t.Errorf("CountStartZeroBits(%x) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
