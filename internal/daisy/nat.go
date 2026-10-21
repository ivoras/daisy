package daisy

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	gonat "github.com/libp2p/go-nat"
)

/*
 * Getting through a NAT router with UPnP or NAT-PMP, if the user asks for it (-upnp).
 *
 * The router is asked to forward its port 2017 (and the HTTP port) to this computer, so that the other nodes can
 * connect to it. Nodes behind a router which doesn't do that still work, they just connect to the others, but
 * nobody can connect to them.
 *
 * The mappings expire, so they are renewed twice per lifetime, and removed when the node exits.
 */

// portMapper is the part of a router that we use
type portMapper interface {
	Type() string
	ExternalAddress() (net.IP, error)
	// AddMapping forwards the port of the router, the same as ours if possible, to ours. Returns the port of the router.
	AddMapping(ctx context.Context, port int, description string, lifetime time.Duration) (externalPort int, err error)
	DeleteMapping(ctx context.Context, port int) error
}

// goNATMapper makes a router found by the go-nat package into a portMapper
type goNATMapper struct{ gonat.NAT }

func (m goNATMapper) ExternalAddress() (net.IP, error) { return m.NAT.GetExternalAddress() }

func (m goNATMapper) AddMapping(ctx context.Context, port int, description string, lifetime time.Duration) (int, error) {
	return m.NAT.AddPortMapping(ctx, "tcp", port, description, lifetime)
}

func (m goNATMapper) DeleteMapping(ctx context.Context, port int) error {
	return m.NAT.DeletePortMapping(ctx, "tcp", port)
}

func discoverRouter(ctx context.Context) (portMapper, error) {
	router, err := gonat.DiscoverGateway(ctx)
	if err != nil {
		return nil, err
	}
	return goNATMapper{router}, nil
}

// What is known about the port mappings
type natStatus struct {
	Enabled    bool        `json:"enabled"`
	Type       string      `json:"type,omitempty"`        // "UPnP (IGDv1-IP1)", "NAT-PMP"...
	ExternalIP string      `json:"external_ip,omitempty"` // The address of the router
	Ports      map[int]int `json:"ports,omitempty"`       // Our port -> the port of the router
	LastError  string      `json:"last_error,omitempty"`
	Renewed    time.Time   `json:"renewed,omitempty"`
}

type natManager struct {
	discover func(ctx context.Context) (portMapper, error)
	ports    map[int]string // The ports to forward -> what they are for
	lifetime time.Duration  // How long a mapping lasts
	retry    time.Duration  // How long to wait before looking for the router again
	timeout  time.Duration  // How long to wait for a router to answer

	lock   sync.Mutex
	status natStatus
	cancel context.CancelFunc
	done   chan struct{}
}

func newNATManager(ports map[int]string) *natManager {
	return &natManager{
		discover: discoverRouter,
		ports:    ports,
		lifetime: time.Hour,
		retry:    10 * time.Minute,
		timeout:  10 * time.Second,
		status:   natStatus{Enabled: true},
	}
}

var natMapper *natManager

func (m *natManager) getStatus() natStatus {
	m.lock.Lock()
	defer m.lock.Unlock()
	s := m.status
	s.Ports = make(map[int]int, len(m.status.Ports))
	for k, v := range m.status.Ports {
		s.Ports[k] = v
	}
	return s
}

func (m *natManager) update(f func(s *natStatus)) {
	m.lock.Lock()
	defer m.lock.Unlock()
	f(&m.status)
}

// start begins to look for the router and to keep the ports mapped, in the background
func (m *natManager) start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		m.run(ctx)
	}()
}

// stop removes the mappings and waits for that to be done
func (m *natManager) stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
	select {
	case <-m.done:
	case <-time.After(10 * time.Second):
		log.Println("Timed out removing the port mappings")
	}
}

// sleep waits, and tells if it was not interrupted
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *natManager) run(ctx context.Context) {
	for {
		dctx, cancel := context.WithTimeout(ctx, m.timeout)
		router, err := m.discover(dctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.update(func(s *natStatus) { s.LastError = "no router found: " + err.Error() })
			log.Printf("No router to forward ports (UPnP/NAT-PMP): %v. Trying again in %v.", err, m.retry)
			if !sleepCtx(ctx, m.retry) {
				return
			}
			continue
		}
		log.Println("Found a router:", router.Type())
		for m.maintain(ctx, router) {
			if !sleepCtx(ctx, m.lifetime/2) {
				m.removeMappings(router)
				return
			}
		}
		if ctx.Err() != nil {
			m.removeMappings(router)
			return
		}
		if !sleepCtx(ctx, m.retry) {
			return
		}
	}
}

// maintain creates or renews the mappings. Returns false if the router has stopped working for us.
func (m *natManager) maintain(ctx context.Context, router portMapper) bool {
	actx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	ext, err := router.ExternalAddress()
	if err != nil {
		m.update(func(s *natStatus) { s.Type, s.LastError = router.Type(), "no external address: "+err.Error() })
		log.Println("The router doesn't tell its external address:", err)
		return false
	}
	ports := make(map[int]int)
	for port, what := range m.ports {
		external, err := router.AddMapping(actx, port, what, m.lifetime)
		if err != nil {
			m.update(func(s *natStatus) {
				s.Type, s.LastError = router.Type(), fmt.Sprintf("cannot map port %d: %v", port, err)
			})
			log.Printf("The router cannot forward the port %d: %v", port, err)
			return false
		}
		if external != port {
			log.Printf("The router forwards its port %d to our port %d (%s). Peers look for nodes on the port %d, so this one might not be found.",
				external, port, what, port)
		}
		ports[port] = external
	}
	extStr := ext.String()
	behindAnotherNAT := isPrivateHost(extStr) || isCGNAT(ext)
	m.update(func(s *natStatus) {
		s.Type, s.ExternalIP, s.Ports, s.LastError, s.Renewed = router.Type(), extStr, ports, "", time.Now()
		if behindAnotherNAT {
			s.LastError = "the router's address " + extStr + " is not a public one: there is another NAT in front of it, and forwarding ports does not help"
		}
	})
	if behindAnotherNAT {
		log.Println("The router's external address", extStr, "is not a public one: there is another NAT in front of it.")
	} else {
		if selfInfo.getExternal() != extStr {
			log.Println("The ports are forwarded by the router, our address is", extStr)
		}
		selfInfo.setExternal(extStr)
	}
	return true
}

// removeMappings asks the router to forget about our ports
func (m *natManager) removeMappings(router portMapper) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for port := range m.getStatus().Ports {
		if err := router.DeleteMapping(ctx, port); err != nil {
			log.Printf("Cannot remove the mapping of the port %d: %v", port, err)
		}
	}
	m.update(func(s *natStatus) { s.Ports = nil })
}

// The addresses 100.64.0.0/10 are for the NATs of the Internet providers
func isCGNAT(ip net.IP) bool {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}
