package snet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/SkycoinProject/skywire-mainnet/pkg/snet/stcp"

	"github.com/SkycoinProject/skycoin/src/util/logging"

	"github.com/SkycoinProject/dmsg"
	"github.com/SkycoinProject/dmsg/cipher"
	"github.com/SkycoinProject/dmsg/disc"
)

// Default ports.
// TODO(evanlinjin): Define these properly. These are currently random.
const (
	SetupPort      = uint16(36)  // Listening port of a setup node.
	AwaitSetupPort = uint16(136) // Listening port of a visor for setup operations.
	TransportPort  = uint16(45)  // Listening port of a visor for incoming transports.
)

// Network types.
const (
	DmsgType = dmsg.Type
	STcpType = stcp.Type
)

var (
	// ErrUnknownNetwork occurs on attempt to dial an unknown network type.
	ErrUnknownNetwork = errors.New("unknown network type")
)

// Config represents a network configuration.
type Config struct {
	PubKey     cipher.PubKey
	SecKey     cipher.SecKey
	TpNetworks []string // networks to be used with transports

	DmsgDiscAddr    string
	DmsgMinSessions int

	STCPLocalAddr string // if empty, don't listen.
	STCPTable     map[cipher.PubKey]string
}

// Network represents a network between nodes in Skywire.
type Network struct {
	conf  Config
	dmsgC *dmsg.Client
	stcpC *stcp.Client
}

// New creates a network from a config.
func New(conf Config) *Network {
	dmsgC := dmsg.NewClient(
		conf.PubKey,
		conf.SecKey,
		disc.NewHTTP(conf.DmsgDiscAddr), &dmsg.Config{
			MinSessions: conf.DmsgMinSessions,
		})
	dmsgC.SetLogger(logging.MustGetLogger("snet.dmsgC"))

	stcpC := stcp.NewClient(
		logging.MustGetLogger("snet.stcpC"),
		conf.PubKey,
		conf.SecKey,
		stcp.NewTable(conf.STCPTable))

	return NewRaw(conf, dmsgC, stcpC)
}

// NewRaw creates a network from a config and a dmsg client.
func NewRaw(conf Config, dmsgC *dmsg.Client, stcpC *stcp.Client) *Network {
	return &Network{
		conf:  conf,
		dmsgC: dmsgC,
		stcpC: stcpC,
	}
}

// Init initiates server connections.
func (n *Network) Init(_ context.Context) error {
	if n.dmsgC != nil {
		time.Sleep(200 * time.Millisecond)
		go n.dmsgC.Serve()
		time.Sleep(200 * time.Millisecond)
	}

	if n.stcpC != nil {
		if n.conf.STCPLocalAddr != "" {
			if err := n.stcpC.Serve(n.conf.STCPLocalAddr); err != nil {
				return fmt.Errorf("failed to initiate 'stcp': %v", err)
			}
		} else {
			fmt.Println("No config found for stcp")
		}
	}

	return nil
}

// Close closes underlying connections.
func (n *Network) Close() error {
	wg := new(sync.WaitGroup)
	wg.Add(2)

	var dmsgErr error
	go func() {
		dmsgErr = n.dmsgC.Close()
		wg.Done()
	}()

	var stcpErr error
	go func() {
		stcpErr = n.stcpC.Close()
		wg.Done()
	}()

	wg.Wait()

	if dmsgErr != nil {
		return dmsgErr
	}
	if stcpErr != nil {
		return stcpErr
	}
	return nil
}

// LocalPK returns local public key.
func (n *Network) LocalPK() cipher.PubKey { return n.conf.PubKey }

// LocalSK returns local secure key.
func (n *Network) LocalSK() cipher.SecKey { return n.conf.SecKey }

// TransportNetworks returns network types that are used for transports.
func (n *Network) TransportNetworks() []string { return n.conf.TpNetworks }

// Dmsg returns underlying dmsg client.
func (n *Network) Dmsg() *dmsg.Client { return n.dmsgC }

// STcp returns the underlying stcp.Client.
func (n *Network) STcp() *stcp.Client { return n.stcpC }

// Dialer is an entity that can be dialed and asked for its type.
type Dialer interface {
	Dial(ctx context.Context, remote cipher.PubKey, port uint16) (net.Conn, error)
	Type() string
}

// Dial dials a visor by its public key and returns a connection.
func (n *Network) Dial(ctx context.Context, network string, pk cipher.PubKey, port uint16) (*Conn, error) {
	switch network {
	case DmsgType:
		addr := dmsg.Addr{
			PK:   pk,
			Port: port,
		}

		conn, err := n.dmsgC.Dial(ctx, addr)
		if err != nil {
			return nil, err
		}

		return makeConn(conn, network)
	case STcpType:
		conn, err := n.stcpC.Dial(ctx, pk, port)
		if err != nil {
			return nil, err
		}

		return makeConn(conn, network)
	default:
		return nil, ErrUnknownNetwork
	}
}

// Listen listens on the specified port.
func (n *Network) Listen(network string, port uint16) (*Listener, error) {
	switch network {
	case DmsgType:
		lis, err := n.dmsgC.Listen(port)
		if err != nil {
			return nil, err
		}

		return makeListener(lis, network)
	case STcpType:
		lis, err := n.stcpC.Listen(port)
		if err != nil {
			return nil, err
		}

		return makeListener(lis, network)
	default:
		return nil, ErrUnknownNetwork
	}
}

// Listener represents a listener.
type Listener struct {
	net.Listener
	lPK     cipher.PubKey
	lPort   uint16
	network string
}

func makeListener(l net.Listener, network string) (*Listener, error) {
	lPK, lPort, err := disassembleAddr(l.Addr())
	if err != nil {
		return nil, err
	}

	return &Listener{Listener: l, lPK: lPK, lPort: lPort, network: network}, nil
}

// LocalPK returns a local public key of listener.
func (l Listener) LocalPK() cipher.PubKey { return l.lPK }

// LocalPort returns a local port of listener.
func (l Listener) LocalPort() uint16 { return l.lPort }

// Network returns a network of listener.
func (l Listener) Network() string { return l.network }

// AcceptConn accepts a connection from listener.
func (l Listener) AcceptConn() (*Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return makeConn(conn, l.network)
}

// Conn represent a connection between nodes in Skywire.
type Conn struct {
	net.Conn
	lPK     cipher.PubKey
	rPK     cipher.PubKey
	lPort   uint16
	rPort   uint16
	network string
}

func makeConn(conn net.Conn, network string) (*Conn, error) {
	lPK, lPort, err := disassembleAddr(conn.LocalAddr())
	if err != nil {
		return nil, err
	}

	rPK, rPort, err := disassembleAddr(conn.RemoteAddr())
	if err != nil {
		return nil, err
	}

	return &Conn{Conn: conn, lPK: lPK, rPK: rPK, lPort: lPort, rPort: rPort, network: network}, nil
}

// LocalPK returns local public key of connection.
func (c Conn) LocalPK() cipher.PubKey { return c.lPK }

// RemotePK returns remote public key of connection.
func (c Conn) RemotePK() cipher.PubKey { return c.rPK }

// LocalPort returns local port of connection.
func (c Conn) LocalPort() uint16 { return c.lPort }

// RemotePort returns remote port of connection.
func (c Conn) RemotePort() uint16 { return c.rPort }

// Network returns network of connection.
func (c Conn) Network() string { return c.network }

func disassembleAddr(addr net.Addr) (pk cipher.PubKey, port uint16, retErr error) {
	strs := strings.Split(addr.String(), ":")
	if len(strs) != 2 {
		retErr = fmt.Errorf("network.disassembleAddr: %v %s", "invalid addr", addr.String())
		return
	}

	if err := pk.Set(strs[0]); err != nil {
		retErr = fmt.Errorf("network.disassembleAddr: %v %s", err, addr.String())
		return
	}

	if strs[1] != "~" {
		if _, err := fmt.Sscanf(strs[1], "%d", &port); err != nil {
			retErr = fmt.Errorf("network.disassembleAddr: %v", err)
			return
		}
	}

	return
}
