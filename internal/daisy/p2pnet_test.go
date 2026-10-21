package daisy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

func TestReadLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("x", 10000)+"\nlast"), 16)
	if line, err := readLine(r, 100); err != nil || string(line) != "short\n" {
		t.Fatalf("got %q, %v", line, err)
	}
	if _, err := readLine(r, 100); !errors.Is(err, errLineTooLong) {
		t.Fatalf("a long line should be refused, got %v", err)
	}

	// A line spanning many buffer fills is fine within the limit, and exactly at the limit
	long := strings.Repeat("y", 999) + "\n"
	r = bufio.NewReaderSize(strings.NewReader(long+long), 16)
	for range 2 {
		if line, err := readLine(r, 1000); err != nil || string(line) != long {
			t.Fatalf("got %d bytes, %v", len(line), err)
		}
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader(long)), 999); !errors.Is(err, errLineTooLong) {
		t.Fatalf("one byte over the limit should be refused, got %v", err)
	}
	if _, err := readLine(r, 1000); err == nil {
		t.Fatal("expected EOF")
	}
}

func TestBlockHTTPURL(t *testing.T) {
	remote4 := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 54321}
	remote6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 54321}
	for _, tc := range []struct {
		name, in string
		remote   net.Addr
		want     string // empty: must be refused
	}{
		{"normal", "http://10.1.2.3:2018/block/5", remote4, "http://203.0.113.7:2018/block/5"},
		{"the host is always the peer's", "http://169.254.169.254:2018/block/5", remote4, "http://203.0.113.7:2018/block/5"},
		{"hostname is replaced too", "http://localhost:2018/block/0", remote4, "http://203.0.113.7:2018/block/0"},
		{"no port", "http://example.com/block/7", remote4, "http://203.0.113.7:80/block/7"},
		{"ipv6 peer", "http://[::1]:2018/block/9", remote6, "http://[2001:db8::1]:2018/block/9"},
		{"https", "https://example.com:2018/block/5", remote4, ""},
		{"file", "file:///etc/passwd", remote4, ""},
		{"userinfo", "http://u:p@example.com:2018/block/5", remote4, ""},
		{"query", "http://example.com:2018/block/5?x=1", remote4, ""},
		{"fragment", "http://example.com:2018/block/5#x", remote4, ""},
		{"other path", "http://example.com:2018/admin", remote4, ""},
		{"traversal", "http://example.com:2018/block/../admin", remote4, ""},
		{"not a number", "http://example.com:2018/block/abc", remote4, ""},
		{"negative", "http://example.com:2018/block/-1", remote4, ""},
		{"port zero", "http://example.com:0/block/1", remote4, ""},
		{"port too large", "http://example.com:70000/block/1", remote4, ""},
		{"garbage", "::::", remote4, ""},
		{"no remote address", "http://example.com:2018/block/1", nil, ""},
	} {
		got, err := blockHTTPURL(tc.in, tc.remote)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: %q should have been refused, got %q", tc.name, tc.in, got)
			}
		} else if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestCheckBlockSize(t *testing.T) {
	for size, ok := range map[int64]bool{
		-1: false, 0: false, 1: true, 1 << 20: true, p2pMaxBlockSize: true, p2pMaxBlockSize + 1: false,
	} {
		if err := checkBlockSize(size); (err == nil) != ok {
			t.Errorf("checkBlockSize(%d) = %v", size, err)
		}
	}
}

func zlibBase64(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestDownloadInlineBlock(t *testing.T) {
	data := bytes.Repeat([]byte("daisy"), 1000)
	var out bytes.Buffer
	if err := downloadInlineBlock(&out, zlibBase64(t, data), int64(len(data))); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("valid block rejected: %v", err)
	}

	// A decompression bomb: it must be stopped at the announced size, not inflated completely
	bomb := zlibBase64(t, make([]byte, 8<<20))
	out.Reset()
	if err := downloadInlineBlock(&out, bomb, 1000); err == nil {
		t.Fatal("block bigger than announced accepted")
	}
	if out.Len() > 1001 {
		t.Errorf("wrote %d bytes of a block announced to be 1000", out.Len())
	}
	out.Reset()
	if err := downloadInlineBlock(&out, zlibBase64(t, data), int64(len(data))+1); err == nil {
		t.Error("block smaller than announced accepted")
	}
	if err := downloadInlineBlock(&out, zlibBase64(t, data), p2pMaxBlockSize+1); err == nil {
		t.Error("announced size over the maximum accepted")
	}
	if err := downloadInlineBlock(&out, "!!! not base64", 10); err == nil {
		t.Error("garbage accepted")
	}
}

func TestDownloadHTTPBlock(t *testing.T) {
	data := bytes.Repeat([]byte("daisy"), 1000)
	var sent atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/block/1":
			w.Write(data)
		case "/block/2": // Much more than announced, without Content-Length (chunked)
			chunk := make([]byte, 64<<10)
			for range 1000 {
				n, err := w.Write(chunk)
				sent.Add(int64(n))
				if err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		case "/block/3":
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	var out bytes.Buffer
	if err := downloadHTTPBlock(&out, ts.URL+"/block/1", int64(len(data))); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("valid block rejected: %v", err)
	}
	out.Reset()
	if err := downloadHTTPBlock(&out, ts.URL+"/block/1", int64(len(data))-1); err == nil {
		t.Error("block bigger than announced accepted (Content-Length)")
	}
	out.Reset()
	if err := downloadHTTPBlock(&out, ts.URL+"/block/2", 1000); err == nil {
		t.Error("block bigger than announced accepted (streamed)")
	}
	if out.Len() > 1001 {
		t.Errorf("wrote %d bytes of a block announced to be 1000", out.Len())
	}
	if err := downloadHTTPBlock(&out, ts.URL+"/block/3", 1000); err == nil {
		t.Error("a 404 response accepted")
	}
}

// A peer must not be able to make us request URLs on other hosts, neither directly nor with a redirect.
func TestHandleBlockDoesNotFetchFromOtherHosts(t *testing.T) {
	setupTestSysDbs(t)
	var victimHits, peerHits atomic.Int32

	// The victim is a different host than the peer: 127.0.0.2 vs. 127.0.0.1
	victimListener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("cannot listen on 127.0.0.2:", err)
	}
	victim := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits.Add(1)
	}))
	victim.Listener.Close()
	victim.Listener = victimListener
	victim.Start()
	defer victim.Close()

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits.Add(1)
		http.Redirect(w, r, victim.URL+"/block/1", http.StatusFound)
	}))
	defer peer.Close()

	conn, err := net.Dial("tcp", peer.Listener.Addr().String()) // RemoteAddr() is the peer
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p2pc := newTestConnection(conn, conn.RemoteAddr().String())
	_, peerPort, _ := net.SplitHostPort(peer.Listener.Addr().String())

	handle := func(url string, size int64) {
		p2pc.handleBlock(util.StrIfMap{
			"hash": fmt.Sprintf("hash-%s", url), "hash_signature": "00", "data": url,
			"size": float64(size), "encoding": "http",
		})
	}

	// 1. A URL for a different host: the host is replaced with the peer's, so the victim is not contacted
	handle(victim.URL+"/block/1", 100)
	if victimHits.Load() != 0 {
		t.Fatal("fetched a URL from a host that isn't the peer")
	}

	// 2. The peer's own port, but it redirects to the victim
	handle("http://example.invalid:"+peerPort+"/block/1", 100)
	if peerHits.Load() != 1 {
		t.Fatalf("expected one request to the peer, got %d", peerHits.Load())
	}
	if victimHits.Load() != 0 {
		t.Fatal("followed a redirect to another host")
	}

	// 3. Absurd sizes are refused before any request is made
	handle("http://example.invalid:"+peerPort+"/block/1", p2pMaxBlockSize+1)
	handle("http://example.invalid:"+peerPort+"/block/1", 0)
	if peerHits.Load() != 1 {
		t.Fatal("made a request for a block of an unacceptable size")
	}
}

func TestBannedPeers(t *testing.T) {
	co := &p2pCoordinatorType{
		badPeers: util.NewStringSetWithExpiry(time.Minute),
		badHosts: util.NewStringSetWithExpiry(time.Minute),
	}
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 2017}
	if co.isBanned(addr) {
		t.Fatal("not banned yet")
	}
	co.badPeers.Add("203.0.113.9:2017")
	if !co.isBanned(addr) {
		t.Error("a banned dial address isn't recognised")
	}
	if co.isBanned(&net.TCPAddr{IP: addr.IP, Port: 2018}) {
		t.Error("a ban of a dial address shouldn't affect other ports")
	}
	co.badHosts.Add("203.0.113.10")
	for _, port := range []int{1, 2017, 50000} {
		if !co.isBanned(&net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: port}) {
			t.Errorf("a banned host should be banned on port %d", port)
		}
	}
}

// Duplicate and self connections: only the outbound ones are remembered as bad dial addresses, because the
// address of an inbound connection contains a random source port.
func TestDuplicateConnectionBans(t *testing.T) {
	setupTestSysDbs(t)
	existing := newTestConnection(nil, "198.51.100.1:2017")
	existing.peerID = 4242
	p2pPeers.Add(existing)
	defer p2pPeers.Remove(existing)

	for _, outbound := range []bool{true, false} {
		local, remote := net.Pipe()
		defer remote.Close()
		address := fmt.Sprintf("198.51.100.2:%d", map[bool]int{true: 2017, false: 40000}[outbound])
		p2pc := newTestConnection(local, address)
		p2pc.outbound = outbound
		hello := util.StrIfMap{"version": "test", "chain_height": float64(0), "p2p_id": float64(4242), "my_peers": []any{}}
		p2pc.handleMsgHello(hello)
		if got := p2pCoordinator.badPeers.Has(address); got != outbound {
			t.Errorf("outbound=%v: badPeers.Has(%s) = %v", outbound, address, got)
		}
	}
}
