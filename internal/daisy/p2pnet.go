package daisy

import (
	"bufio"
	"errors"
	"fmt"
	"net"
)

// Size limits for data received from peers. They are generous (a block is a whole SQLite database)
// but finite, so a peer can't make us run out of memory or disk.
const (
	// p2pMaxBlockSize is the largest block (SQLite file) accepted from the network, and created locally.
	p2pMaxBlockSize = 1 << 30

	// p2pMaxMessageSize is the longest single p2p message (one JSON line) accepted from a peer.
	p2pMaxMessageSize = 64 << 20
)

var errLineTooLong = errors.New("message too long")

// readLine reads one '\n'-terminated line (the newline is included) of at most maxLen bytes.
func readLine(r *bufio.Reader, maxLen int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxLen {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// remoteHost returns the host (IP address) part of a network address
func remoteHost(addr net.Addr) (string, error) {
	if addr == nil {
		return "", errors.New("no remote address")
	}
	host, _, err := net.SplitHostPort(addr.String())
	return host, err
}

// checkBlockSize validates the size of a block announced by a peer
func checkBlockSize(size int64) error {
	if size <= 0 || size > p2pMaxBlockSize {
		return fmt.Errorf("unacceptable block size %d (maximum is %d)", size, int64(p2pMaxBlockSize))
	}
	return nil
}
