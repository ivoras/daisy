package daisy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Size limits for data received from peers. They are generous (a block is a whole SQLite database)
// but finite, so a peer can't make us run out of memory or disk.
const (
	// p2pMaxBlockSize is the largest block (SQLite file) accepted from the network, and created locally.
	p2pMaxBlockSize = 1 << 30

	// p2pMaxMessageSize is the longest single p2p message (one JSON line) accepted from a peer.
	p2pMaxMessageSize = 64 << 20

	// Blocks larger than this are always sent over HTTP, even when inline sending is requested,
	// so the message stays within p2pMaxMessageSize after compression and base64 encoding.
	p2pMaxInlineBlockSize = 32 << 20
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

var blockURLPathRegexp = regexp.MustCompile(`^/block/[0-9]{1,9}$`)

// blockHTTPURL validates the block URL a peer has sent, and returns the URL we are willing to fetch.
//
// The peer only gets to say which port and which block: the host is always replaced with the address
// of the peer itself, as seen on the p2p connection. Otherwise a peer could make us send HTTP requests
// to arbitrary hosts (including ones on our private network).
func blockHTTPURL(rawURL string, remote net.Addr) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" {
		return "", fmt.Errorf("unsupported block URL scheme %q", u.Scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("unexpected parts in the block URL")
	}
	if !blockURLPathRegexp.MatchString(u.Path) {
		return "", fmt.Errorf("unexpected block URL path %q", u.Path)
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid block URL port %q", port)
	}
	host, err := remoteHost(remote)
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(host, port) + u.Path, nil
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

// copyExactly copies exactly size bytes from src to dst. It reads at most one byte more than that
// so that a source that lies about its size can't make us consume more than we agreed to.
func copyExactly(dst io.Writer, src io.Reader, size int64) error {
	written, err := io.Copy(dst, io.LimitReader(src, size+1))
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("sizes don't match: %d vs %d", written, size)
	}
	return nil
}

// downloadInlineBlock decodes a block sent inline ("zlib-base64" encoding) into dst.
func downloadInlineBlock(dst io.Writer, data string, size int64) error {
	if err := checkBlockSize(size); err != nil {
		return err
	}
	zlibData, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return err
	}
	r, err := zlib.NewReader(bytes.NewReader(zlibData))
	if err != nil {
		return err
	}
	defer r.Close()
	return copyExactly(dst, r, size)
}

// downloadHTTPBlock fetches a block (the "http" encoding) from the given, already validated, URL into dst.
func downloadHTTPBlock(dst io.Writer, blockURL string, size int64) error {
	if err := checkBlockSize(size); err != nil {
		return err
	}
	resp, err := p2pHTTPClient.Get(blockURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %s", resp.Status)
	}
	if resp.ContentLength > size {
		return fmt.Errorf("the server sends more data than announced: %d vs %d", resp.ContentLength, size)
	}
	return copyExactly(dst, resp.Body, size)
}
