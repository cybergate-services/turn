package relayServer

import (
	"fmt"
	"net"
	"sync"

	"github.com/pions/pkg/stun"
	"github.com/pkg/errors"
	"golang.org/x/net/ipv4"
)

// Public
type Permission struct {
	IP           net.IP
	Port         int
	TimeToExpiry uint32
}

type Protocol int

const (
	UDP Protocol = iota
	TCP Protocol = iota
)

type FiveTuple struct {
	SrcAddr  *stun.TransportAddr
	DstAddr  *stun.TransportAddr
	Protocol Protocol
}

func (a *FiveTuple) match(b *FiveTuple) bool {
	return a.SrcAddr.Equal(b.SrcAddr) &&
		a.DstAddr.Equal(b.DstAddr) &&
		a.Protocol == b.Protocol
}

type ChannelBind struct {
	addr *stun.TransportAddr
	// expiration uint32
}

func Start(fiveTuple *FiveTuple, reservationToken string, lifetime uint32, username string) (listeningPort int, err error) {
	s := &server{
		FiveTuple:        fiveTuple,
		reservationToken: reservationToken,
		lifetime:         lifetime,
		channelBindings:  map[uint16]ChannelBind{},
	}

	listener, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return
	}
	listeningAddr, err := stun.NewTransportAddr(listener.LocalAddr())
	if err != nil {
		return
	}

	listeningPort = listeningAddr.Port
	s.listeningPort = listeningPort
	s.username = username

	serversLock.Lock()
	servers = append(servers, s)
	serversLock.Unlock()

	go relayHandler(s, listener)
	return
}

//Caller must unlock mutex
func getServer(fiveTuple *FiveTuple) (server *server) {
	serversLock.RLock()

	for _, s := range servers {
		if fiveTuple.match(s.FiveTuple) {
			server = s
		}
	}
	return
}

func Fulfilled(fiveTuple *FiveTuple) bool {
	defer serversLock.RUnlock()
	return getServer(fiveTuple) != nil
}

func AddPermission(fiveTuple *FiveTuple, permission *Permission) error {
	s := getServer(fiveTuple)
	serversLock.RUnlock()
	if s == nil {
		return errors.Errorf("Unable to add permission, server not found")
	}
	s.permissionsLock.Lock()
	defer s.permissionsLock.Unlock()
	for _, p := range s.permissions {
		if p.Port == permission.Port && p.IP.Equal(permission.IP) {
			return nil
		}
	}

	s.permissions = append(s.permissions, permission)
	return nil
}

func GetSrcForRelay(addr *stun.TransportAddr) (*stun.TransportAddr, error) {
	serversLock.RLock()
	defer serversLock.RUnlock()

	for _, s := range servers {
		if addr.Port == s.listeningPort {
			return s.FiveTuple.SrcAddr, nil
		}
	}

	return nil, errors.Errorf("No Relay is listening on port %d", addr.Port)
}

func GetRelayForSrc(addr *stun.TransportAddr) (int, error) {
	serversLock.RLock()
	defer serversLock.RUnlock()

	for _, s := range servers {
		if s.FiveTuple.SrcAddr.Equal(addr) {
			return s.listeningPort, nil
		}
	}

	return 0, errors.Errorf("No Relay is allocated to this src %d", addr.Port)
}

func AddChannelBind(relayPort int, channel uint16, dstAddr *stun.TransportAddr) error {
	serversLock.RLock()
	defer serversLock.RUnlock()
	for _, s := range servers {
		if s.listeningPort == relayPort {
			s.channelBindings[channel] = ChannelBind{addr: dstAddr}
		}
	}
	return nil
}

func GetChannelBind(srcPort int, channel uint16) (*stun.TransportAddr, bool) {
	serversLock.RLock()
	defer serversLock.RUnlock()
	for _, s := range servers {
		if cb, ok := s.channelBindings[channel]; ok && cb.addr.Port == srcPort {
			return s.FiveTuple.SrcAddr, true
		}
	}

	return nil, false
}

// Private
type server struct {
	*FiveTuple
	listeningPort              int
	reservationToken, username string
	lifetime                   uint32
	permissionsLock            sync.RWMutex
	permissions                []*Permission
	channelBindings            map[uint16]ChannelBind
}

var serversLock sync.RWMutex
var servers []*server

const RtpMTU = 1500

//  https://tools.ietf.org/html/rfc5766#section-10.3
//  When the server receives a UDP datagram at a currently allocated
//  relayed transport address, the server looks up the allocation
//  associated with the relayed transport address.  The server then
//  checks to see whether the set of permissions for the allocation allow
//  the relaying of the UDP datagram as described in Section 8.
//
//  If relaying is permitted, then the server checks if there is a
//  channel bound to the peer that sent the UDP datagram (see
//  Section 11).  If a channel is bound, then processing proceeds as
//  described in Section 11.7.
//
//  If relaying is permitted but no channel is bound to the peer, then
//  the server forms and sends a Data indication.  The Data indication
//  MUST contain both an XOR-PEER-ADDRESS and a DATA attribute.  The DATA
//  attribute is set to the value of the 'data octets' field from the
//  datagram, and the XOR-PEER-ADDRESS attribute is set to the source
//  transport address of the received UDP datagram.  The Data indication
//  is then sent on the 5-tuple associated with the allocation.
func relayHandler(s *server, l net.PacketConn) {
	buffer := make([]byte, RtpMTU)
	conn := ipv4.NewPacketConn(l)

	dataAttr := stun.Data{}
	xorPeerAddressAttr := stun.XorPeerAddress{}

	for {
		n, srcAddr, err := l.ReadFrom(buffer)
		if err != nil {
			fmt.Println("Failing to relay")
		}

		xorPeerAddressAttr.XorAddress.IP = srcAddr.(*net.UDPAddr).IP
		xorPeerAddressAttr.XorAddress.Port = srcAddr.(*net.UDPAddr).Port
		dataAttr.Data = buffer[:n]

		_ = stun.BuildAndSend(conn, s.FiveTuple.SrcAddr, stun.ClassIndication, stun.MethodData, buildTransactionId(), &xorPeerAddressAttr, &dataAttr)
		// fmt.Printf("Relaying %d %s %s %d \n", s.listeningPort, srcAddr.String(), s.FiveTuple.SrcAddr, n)
	}
}
