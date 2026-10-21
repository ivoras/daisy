package daisy

import (
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ivoras/daisy/internal/util"
)

/*
 * The address book: the peers which we know about, and which we may connect to.
 *
 * It is made to be hard to fill with addresses chosen by somebody else, and to resist a few malicious peers
 * that want to be the only peers we know (an "eclipse" attack):
 *
 *  - There are two tiers. The "new" addresses are those which we have heard of from a peer or a seed, but never
 *    connected to. The "tried" addresses are those of peers which have said hello to us.
 *  - Each network group (the /16 of an IPv4 address, the /32 of an IPv6 address) can have only a few addresses
 *    in the book, and no peer, seed or other source can have told us about more than a limited number of the new
 *    addresses, so a single party can't take over the book.
 *  - The outbound connections are made to the addresses of different network groups, chosen randomly from
 *    the tried and from the new ones, and a peer which keeps failing is tried less and less often.
 *
 * The addresses of private networks (and the addresses of the bootstrap peers, which are configured by whoever
 * made the chain) are not subject to the limits per group, otherwise a company network, where all the nodes
 * are in one /16, could never connect them all.
 */
var (
	abMaxNew           = 1024
	abMaxTried         = 256
	abMaxNewPerGroup   = 8
	abMaxTriedPerGroup = 4
	abMaxNewPerSource  = 64
)

// A peer is given up on (unless it's a bootstrap peer) after this many failed connection attempts in a row
const peerMaxFailures = 5

// ... or when it hasn't been heard of for this long
const peerMaxAge = 30 * 24 * time.Hour

var abLock sync.Mutex

// A variable so that the tests can choose
var abRandFloat = rand.Float64

// isPrivateHost tells if the host is an address (or name) which is not reachable from the Internet
func isPrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost")
}

// networkGroup returns the group of the networks which the host belongs to, or "" if no group limits apply to it
func networkGroup(host string) string {
	if isPrivateHost(host) {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "dns:" + strings.ToLower(host)
	}
	if ip4 := ip.To4(); ip4 != nil {
		return fmt.Sprintf("v4:%d.%d", ip4[0], ip4[1])
	}
	return fmt.Sprintf("v6:%02x%02x:%02x%02x", ip[0], ip[1], ip[2], ip[3])
}

// sourceGroup returns the identity of whoever has told us about an address: the group of the network of a peer,
// or the name of a kind of sources ("seed", "mdns"...).
func sourceGroup(source string) string {
	if ip := net.ParseIP(source); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return fmt.Sprintf("v4:%d.%d", ip4[0], ip4[1])
		}
		return fmt.Sprintf("v6:%02x%02x:%02x%02x", ip[0], ip[1], ip[2], ip[3])
	}
	return source
}

// splitPeerAddress validates an address of a peer in the "host:port" form
func splitPeerAddress(address string) (host string, ok bool) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil || host == "" || len(host) > 255 {
		return "", false
	}
	if port, err := strconv.Atoi(portStr); err != nil || port < 1 || port > 65535 {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
		return "", false
	}
	return host, true
}

// abAdd remembers an address that we have heard of, as a new one. Returns true if it has been added. The source is
// who has told us about it: the address of a peer, or the name of a kind of sources.
func abAdd(address, source string) bool {
	host, ok := splitPeerAddress(address)
	if !ok {
		return false
	}
	group, srcGroup := networkGroup(host), sourceGroup(source)

	abLock.Lock()
	defer abLock.Unlock()
	if abExists(address) {
		return false
	}
	if group != "" && abCount("tried=0 AND net_group=?", group) >= abMaxNewPerGroup {
		return false
	}
	if srcGroup != "" && abCount("tried=0 AND source_group=?", srcGroup) >= abMaxNewPerSource {
		return false
	}
	if abCount("tried=0") >= abMaxNew && !abEvictNew() {
		return false
	}
	_, err := mainDb.Exec("INSERT INTO peers(address, time_added, net_group, source_group) VALUES (?, ?, ?, ?)",
		address, util.NowUTC(), group, srcGroup)
	if err != nil {
		log.Println("abAdd:", err)
		return false
	}
	return true
}

// abAddBootstrap adds the address of a bootstrap peer, configured by whoever has made the chain. It's never forgotten.
func abAddBootstrap(address string) {
	host, ok := splitPeerAddress(address)
	if !ok {
		log.Println("Ignoring the invalid bootstrap peer address", address)
		return
	}
	abLock.Lock()
	defer abLock.Unlock()
	_, err := mainDb.Exec(`INSERT INTO peers(address, time_added, permanent, tried, net_group, source_group) VALUES (?, ?, 1, 1, ?, 'bootstrap')
		ON CONFLICT(address) DO UPDATE SET permanent=1, tried=1`, address, util.NowUTC(), networkGroup(host))
	if err != nil {
		log.Panic(err)
	}
}

// abGood records that the address belongs to a real peer, one that has said hello. Returns true if that's new.
func abGood(address string) bool {
	host, ok := splitPeerAddress(address)
	if !ok {
		return false
	}
	group := networkGroup(host)
	abLock.Lock()
	defer abLock.Unlock()
	var wasTried bool
	err := mainDb.QueryRow("SELECT tried FROM peers WHERE address=?", address).Scan(&wasTried)
	now := util.NowUTC()
	if err != nil {
		_, err = mainDb.Exec("INSERT INTO peers(address, time_added, tried, net_group, source_group, last_try, last_success) VALUES (?, ?, 1, ?, 'hello', ?, ?)",
			address, now, group, now, now)
	} else {
		_, err = mainDb.Exec("UPDATE peers SET tried=1, time_added=?, failures=0, last_try=?, last_success=? WHERE address=?", now, now, now, address)
	}
	if err != nil {
		log.Println("abGood:", err)
		return false
	}
	abLimitTried(address, group)
	return !wasTried
}

// abFailed records that connecting to the address has failed
func abFailed(address string) {
	if _, err := mainDb.Exec("UPDATE peers SET failures=failures+1, last_try=? WHERE address=?", util.NowUTC(), address); err != nil {
		log.Println("abFailed:", err)
	}
}

// abTry records that we are about to connect to the address
func abTry(address string) {
	if _, err := mainDb.Exec("UPDATE peers SET last_try=? WHERE address=?", util.NowUTC(), address); err != nil {
		log.Println("abTry:", err)
	}
}

// abForget removes an address which turned out to be our own
func abForget(address string) {
	abLock.Lock()
	defer abLock.Unlock()
	if _, err := mainDb.Exec("DELETE FROM peers WHERE address=? AND NOT permanent", address); err != nil {
		log.Println("abForget:", err)
	}
}

// Must be called with abLock held
func abExists(address string) bool {
	return abCount("address=?", address) > 0
}

func abCount(where string, args ...any) int {
	var n int
	if err := mainDb.QueryRow("SELECT COUNT(*) FROM peers WHERE "+where, args...).Scan(&n); err != nil {
		log.Panic(err)
	}
	return n
}

// abEvictNew removes the least promising new address, to make room. Must be called with abLock held.
func abEvictNew() bool {
	res, err := mainDb.Exec(`DELETE FROM peers WHERE address = (SELECT address FROM peers WHERE tried=0 AND NOT permanent
		ORDER BY failures DESC, time_added ASC LIMIT 1)`)
	if err != nil {
		log.Println("abEvictNew:", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// abLimitTried moves the oldest of the tried addresses back among the new ones, if there are too many of them.
// Must be called with abLock held.
func abLimitTried(keep, group string) {
	demote := func(where string, args ...any) {
		args = append(args, keep)
		_, err := mainDb.Exec(`UPDATE peers SET tried=0 WHERE address = (SELECT address FROM peers WHERE tried=1 AND NOT permanent AND `+where+`
			AND address != ? ORDER BY last_success ASC LIMIT 1)`, args...)
		if err != nil {
			log.Println("abLimitTried:", err)
		}
	}
	if group != "" {
		for abCount("tried=1 AND net_group=?", group) > abMaxTriedPerGroup {
			before := abCount("tried=1 AND net_group=?", group)
			demote("net_group=?", group)
			if abCount("tried=1 AND net_group=?", group) == before {
				break // Nothing can be demoted: the others are permanent
			}
		}
	}
	for abCount("tried=1") > abMaxTried {
		before := abCount("tried=1")
		demote("1=1")
		if abCount("tried=1") == before {
			break
		}
	}
}

// abPrune forgets the addresses of the peers which are dead or were heard of long ago, and trims the book to its size.
func abPrune() {
	abLock.Lock()
	defer abLock.Unlock()
	_, err := mainDb.Exec("DELETE FROM peers WHERE NOT permanent AND (failures >= ? OR time_added < ?)",
		peerMaxFailures, time.Now().Add(-peerMaxAge).Unix())
	if err != nil {
		log.Println("abPrune:", err)
	}
	for abCount("tried=0") > abMaxNew {
		if !abEvictNew() {
			break
		}
	}
	abLimitTried("", "")
}

// abCounts returns the numbers of the new and of the tried addresses
func abCounts() (newAddresses, tried int) {
	return abCount("tried=0"), abCount("tried=1")
}

// abList returns the addresses of the peers that we have talked to and are the most likely to be alive, to tell others about
func abList(limit int) []string {
	rows, err := mainDb.Query("SELECT address FROM peers WHERE tried=1 AND last_success > 0 AND failures < ? ORDER BY last_success DESC LIMIT ?", peerMaxFailures, limit)
	if err != nil {
		log.Println("abList:", err)
		return nil
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			result = append(result, a)
		}
	}
	return result
}

// How long to wait before trying an address again, given how many times in a row it has failed
func abBackoff(failures int) time.Duration {
	return min(time.Hour, 30*time.Second<<min(failures, 7))
}

// abSelect chooses up to n addresses to connect to. The addresses are chosen at random, from the tried and
// from the new ones alike, they are in different network groups (and not in the groups to avoid), and the ones
// that have recently been tried, or have failed repeatedly, are left out. The skip function leaves out more of them.
func abSelect(n int, avoidGroups map[string]bool, skip func(address string) bool) []string {
	type candidate struct{ address, group string }
	var tried, fresh []candidate

	abLock.Lock()
	rows, err := mainDb.Query("SELECT address, tried, net_group, failures, last_try FROM peers WHERE failures < ? OR permanent", peerMaxFailures)
	if err != nil {
		abLock.Unlock()
		log.Println("abSelect:", err)
		return nil
	}
	now := time.Now()
	for rows.Next() {
		var c candidate
		var isTried bool
		var failures int
		var lastTry int64
		if rows.Scan(&c.address, &isTried, &c.group, &failures, &lastTry) != nil {
			continue
		}
		if now.Sub(time.Unix(lastTry, 0)) < abBackoff(failures) {
			continue
		}
		if isTried {
			tried = append(tried, c)
		} else {
			fresh = append(fresh, c)
		}
	}
	rows.Close()
	abLock.Unlock()

	rand.Shuffle(len(tried), func(i, j int) { tried[i], tried[j] = tried[j], tried[i] })
	rand.Shuffle(len(fresh), func(i, j int) { fresh[i], fresh[j] = fresh[j], fresh[i] })

	used := make(map[string]bool)
	var result []string
	for len(result) < n && (len(tried) > 0 || len(fresh) > 0) {
		list := &fresh
		if (abRandFloat() < 0.5 && len(tried) > 0) || len(fresh) == 0 {
			list = &tried
		}
		c := (*list)[0]
		*list = (*list)[1:]
		if c.group != "" && (avoidGroups[c.group] || used[c.group]) {
			continue
		}
		if skip != nil && skip(c.address) {
			continue
		}
		if c.group != "" {
			used[c.group] = true
		}
		result = append(result, c.address)
	}
	return result
}
