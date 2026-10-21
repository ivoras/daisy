package daisy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

/*
 * Finding peers: the DNS seeds and the lists of peers, which the creator of a chain can put in its chainparams.json.
 *
 * Whatever they tell is just a set of addresses to try, which go to the address book as new addresses: the peers
 * are real only after they have said hello for this chain, so a wrong answer (or a poisoned DNS) can at worst
 * waste a few connection attempts.
 */

// A list of peers published over HTTP: either {"peers": ["host:port", ...]}, a JSON array, or one address per line.
const maxPeerListSize = 1 << 20

var peerListClient = &http.Client{Timeout: 15 * time.Second}

// fetchPeerList downloads a list of peers
func fetchPeerList(listURL string) ([]string, error) {
	u, err := url.Parse(listURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%q is not an HTTP URL", listURL)
	}
	resp, err := peerListClient.Get(listURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPeerListSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPeerListSize {
		return nil, errors.New("the list is too large")
	}
	return parsePeerList(body), nil
}

// parsePeerList understands the formats of the lists of peers. The entries without a port get the default one.
func parsePeerList(body []byte) []string {
	var entries []string
	var object struct {
		Peers []string `json:"peers"`
	}
	if json.Unmarshal(body, &object) == nil && object.Peers != nil {
		entries = object.Peers
	} else if json.Unmarshal(body, &entries) != nil {
		entries = nil
		for _, line := range strings.Split(string(body), "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			if line = strings.TrimSpace(line); line != "" {
				entries = append(entries, line)
			}
		}
	}
	var result []string
	for _, e := range entries {
		if host, port, ok := splitHostPortDefault(e); ok {
			result = append(result, net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	return result
}

// resolveSeed turns the name of a DNS seed ("host" or "host:port") into the addresses of peers
func resolveSeed(seed string) []string {
	host, port, ok := splitHostPortDefault(seed)
	if !ok {
		log.Println("Invalid DNS seed", seed)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		log.Println("Cannot resolve the DNS seed", seed+":", err)
		return nil
	}
	var result []string
	for _, ip := range ips {
		result = append(result, net.JoinHostPort(ip, strconv.Itoa(port)))
	}
	return result
}

// discoverPeers asks the seeds and the lists of the chain for peers, if we haven't got enough of them. It blocks.
func (co *p2pCoordinatorType) discoverPeers() {
	if p2pPeers.Count(true) >= p2pTargetOutbound || (len(chainParams.DNSSeeds) == 0 && len(chainParams.PeerListURLs) == 0) {
		return
	}
	var addresses []string
	for _, seed := range chainParams.DNSSeeds {
		addresses = append(addresses, resolveSeed(seed)...)
	}
	for _, listURL := range chainParams.PeerListURLs {
		list, err := fetchPeerList(listURL)
		if err != nil {
			log.Println("Cannot get the list of peers", listURL+":", err)
			continue
		}
		addresses = append(addresses, list...)
	}
	rand.Shuffle(len(addresses), func(i, j int) { addresses[i], addresses[j] = addresses[j], addresses[i] })
	if len(addresses) > 0 {
		log.Println("Found", len(addresses), "addresses of peers from the seeds and the lists")
		p2pCtrlSend(p2pCtrlMessage{msgType: p2pCtrlConnectPeers, payload: p2pConnectPeersRequest{
			addresses: addresses, source: "seed", allowPrivate: true, trusted: true, keepPorts: true}})
	}
}
