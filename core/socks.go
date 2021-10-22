package core

import (
	"bytes"
	"context"
	"fmt"
	"github.com/wencaiwulue/kubevpn/util"
	"net"
	"strconv"
	"time"

	"github.com/ginuerzh/gosocks5"
	log "github.com/sirupsen/logrus"
)

type fakeUDPTunConnector struct {
}

// SOCKS5UDPTunConnector creates a connector for SOCKS5 UDP-over-TCP relay.
// It accepts an optional auth info for SOCKS5 Username/Password Authentication.
func SOCKS5UDPTunConnector() Connector {
	return &fakeUDPTunConnector{}
}

func (c *fakeUDPTunConnector) ConnectContext(_ context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}
	_ = conn.SetDeadline(time.Now().Add(util.ConnectTimeout))
	defer conn.SetDeadline(time.Time{})

	targetAddr, _ := net.ResolveUDPAddr("udp", address)
	return newFakeUDPTunnelConnOverTcp(conn, targetAddr)
}

type fakeUdpHandler struct {
}

// SOCKS5Handler creates a server Handler for SOCKS5 proxy server.
func SOCKS5Handler() Handler {
	h := &fakeUdpHandler{}
	h.Init()

	return h
}

func (h *fakeUdpHandler) Init(...HandlerOption) {
}

func (h *fakeUdpHandler) Handle(conn net.Conn) {
	defer conn.Close()
	if util.Debug {
		log.Debugf("[socks5] %s -> %s\n", conn.RemoteAddr(), conn.LocalAddr())
	}
	h.handleUDPTunnel(conn)
}

func (h *fakeUdpHandler) transportUDP(relay, peer net.PacketConn) (err error) {
	errc := make(chan error, 2)

	var clientAddr net.Addr

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, laddr, err := relay.ReadFrom(b)
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				clientAddr = laddr
			}
			dgram, err := gosocks5.ReadUDPDatagram(bytes.NewReader(b[:n]))
			if err != nil {
				errc <- err
				return
			}

			raddr, err := net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
			if err != nil {
				continue // drop silently
			}
			if _, err := peer.WriteTo(dgram.Data, raddr); err != nil {
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5-udp] %s >>> %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
			}
		}
	}()

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, raddr, err := peer.ReadFrom(b)
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				continue
			}
			buf := bytes.Buffer{}
			dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(0, 0, toSocksAddr(raddr)), b[:n])
			_ = dgram.Write(&buf)
			if _, err := relay.WriteTo(buf.Bytes(), clientAddr); err != nil {
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5-udp] %s <<< %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
			}
		}
	}()

	return <-errc
}

func (h *fakeUdpHandler) tunnelClientUDP(uc *net.UDPConn, cc net.Conn) (err error) {
	errc := make(chan error, 2)

	var clientAddr *net.UDPAddr

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, addr, err := uc.ReadFromUDP(b)
			if err != nil {
				log.Debugf("[udp-tun] %s <- %s : %s", cc.RemoteAddr(), addr, err)
				errc <- err
				return
			}

			// glog.V(LDEBUG).Infof("read udp %d, % #x", n, b[:n])
			// pipe from relay to tunnel
			dgram, err := gosocks5.ReadUDPDatagram(bytes.NewReader(b[:n]))
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				clientAddr = addr
			}
			//raddr := dgram.Header.Addr.String()
			dgram.Header.Rsv = uint16(len(dgram.Data))
			if err := dgram.Write(cc); err != nil {
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[udp-tun] %s >>> %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				log.Debugf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from tunnel to relay
			if clientAddr == nil {
				continue
			}
			//raddr := dgram.Header.Addr.String()
			dgram.Header.Rsv = 0

			buf := bytes.Buffer{}
			_ = dgram.Write(&buf)
			if _, err := uc.WriteToUDP(buf.Bytes(), clientAddr); err != nil {
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[udp-tun] %s <<< %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	return <-errc
}

func (h *fakeUdpHandler) handleUDPTunnel(conn net.Conn) {
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	bindAddr, _ := net.ResolveUDPAddr("udp", ":0")
	uc, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		log.Debugf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), bindAddr, err)
		return
	}
	defer uc.Close()
	if util.Debug {
		log.Debugf("[socks5] udp-tun %s <- %s\n", conn.RemoteAddr(), uc.LocalAddr())
	}
	log.Debugf("[socks5] udp-tun %s <-> %s", conn.RemoteAddr(), uc.LocalAddr())
	_ = h.tunnelServerUDP(conn, uc)
	log.Debugf("[socks5] udp-tun %s >-< %s", conn.RemoteAddr(), uc.LocalAddr())
	return
}

func (h *fakeUdpHandler) tunnelServerUDP(cc net.Conn, pc net.PacketConn) (err error) {
	errc := make(chan error, 2)

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				// log.Debugf("[udp-tun] %s : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from peer to tunnel
			dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(uint16(n), 0, toSocksAddr(addr)), b[:n])
			if err := dgram.Write(cc); err != nil {
				log.Debugf("[socks5] udp-tun %s <- %s : %s", cc.RemoteAddr(), dgram.Header.Addr, err)
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5] udp-tun %s <<< %s length: %d", cc.RemoteAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				// log.Debugf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from tunnel to peer
			addr, err := net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
			if err != nil {
				continue // drop silently
			}
			if _, err := pc.WriteTo(dgram.Data, addr); err != nil {
				log.Debugf("[socks5] udp-tun %s -> %s : %s", cc.RemoteAddr(), addr, err)
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5] udp-tun %s >>> %s length: %d", cc.RemoteAddr(), addr, len(dgram.Data))
			}
		}
	}()

	return <-errc
}

func toSocksAddr(addr net.Addr) *gosocks5.Addr {
	host := "0.0.0.0"
	port := 0
	if addr != nil {
		h, p, _ := net.SplitHostPort(addr.String())
		host = h
		port, _ = strconv.Atoi(p)
	}
	return &gosocks5.Addr{
		Type: gosocks5.AddrIPv4,
		Host: host,
		Port: uint16(port),
	}
}

// fake upd connect over tcp
type fakeUDPTunnelConn struct {
	// tcp connection
	net.Conn
	targetAddr net.Addr
}

func newFakeUDPTunnelConnOverTcp(conn net.Conn, targetAddr net.Addr) (net.Conn, error) {
	return &fakeUDPTunnelConn{
		Conn:       conn,
		targetAddr: targetAddr,
	}, nil
}

func (c *fakeUDPTunnelConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *fakeUDPTunnelConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	dgram, err := gosocks5.ReadUDPDatagram(c.Conn)
	if err != nil {
		return
	}
	n = copy(b, dgram.Data)
	addr, err = net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
	return
}

func (c *fakeUDPTunnelConn) Write(b []byte) (n int, err error) {
	return c.WriteTo(b, c.targetAddr)
}

func (c *fakeUDPTunnelConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(uint16(len(b)), 0, toSocksAddr(addr)), b)
	if err = dgram.Write(c.Conn); err != nil {
		return
	}
	return len(b), nil
}
