// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package p2p implements the Ethereum p2p network protocols.
package p2p

import (
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/teamnsrg/go-ethereum/common"
	"github.com/teamnsrg/go-ethereum/common/mclock"
	"github.com/teamnsrg/go-ethereum/common/mticker"
	"github.com/teamnsrg/go-ethereum/crypto"
	"github.com/teamnsrg/go-ethereum/event"
	"github.com/teamnsrg/go-ethereum/log"
	"github.com/teamnsrg/go-ethereum/p2p/discover"
	"github.com/teamnsrg/go-ethereum/p2p/discv5"
	"github.com/teamnsrg/go-ethereum/p2p/nat"
	"github.com/teamnsrg/go-ethereum/p2p/netutil"
)

const (
	defaultDialTimeout = 15 * time.Second

	// Maximum number of concurrently handshaking inbound connections.
	maxAcceptConns = 50

	// Maximum time allowed for reading a complete message.
	// This is effectively the amount of time a connection can be idle.
	frameReadTimeout = 30 * time.Second

	// Maximum amount of time allowed for writing a complete message.
	frameWriteTimeout = 20 * time.Second
)

var errServerStopped = errors.New("server stopped")

// Config holds Server options.
type Config struct {
	// MaxDial is the maximum number of concurrently dialing outbound connections.
	MaxDial int

	// NoMaxPeers ignores/overwrites MaxPeers, allowing unlimited number of peer connections.
	NoMaxPeers bool

	// Blacklist is the list of IP networks that we should not connect to
	Blacklist *netutil.Netlist `toml:",omitempty"`

	// DialFreq is the frequency of re-dialing static nodes (in seconds).
	DialFreq int

	// DialCheckFreq is the frequency of checking static nodes ready for redial (in seconds).
	DialCheckFreq int

	// PushFreq is the frequency of pushing updates to MySQL database (in seconds).
	PushFreq int

	// MySQLName is the MySQL node database connection information
	MySQLName string

	// BackupSQL makes a backup of the current MySQL db tables.
	BackupSQL bool

	// ResetSQL makes a backup of the current MySQL db tables and resets them.
	// If set true, BackupSQL should be set true as well.
	ResetSQL bool

	// This field must be set to a valid secp256k1 private key.
	PrivateKey *ecdsa.PrivateKey `toml:"-"`

	// MaxPeers is the maximum number of peers that can be
	// connected. It must be greater than zero.
	MaxPeers int

	// MaxPendingPeers is the maximum number of peers that can be pending in the
	// handshake phase, counted separately for inbound and outbound connections.
	// Zero defaults to preset values.
	MaxPendingPeers int `toml:",omitempty"`

	// NoDiscovery can be used to disable the peer discovery mechanism.
	// Disabling is useful for protocol debugging (manual topology).
	NoDiscovery bool

	// DiscoveryV5 specifies whether the the new topic-discovery based V5 discovery
	// protocol should be started or not.
	DiscoveryV5 bool `toml:",omitempty"`

	// Listener address for the V5 discovery protocol UDP traffic.
	DiscoveryV5Addr string `toml:",omitempty"`

	// Name sets the node name of this server.
	// Use common.MakeName to create a name that follows existing conventions.
	Name string `toml:"-"`

	// BootstrapNodes are used to establish connectivity
	// with the rest of the network.
	BootstrapNodes []*discover.Node

	// BootstrapNodesV5 are used to establish connectivity
	// with the rest of the network using the V5 discovery
	// protocol.
	BootstrapNodesV5 []*discv5.Node `toml:",omitempty"`

	// Static nodes are used as pre-configured connections which are always
	// maintained and re-connected on disconnects.
	StaticNodes []*discover.Node

	// Trusted nodes are used as pre-configured connections which are always
	// allowed to connect, even above the peer limit.
	TrustedNodes []*discover.Node

	// Connectivity can be restricted to certain IP networks.
	// If this option is set to a non-nil value, only hosts which match one of the
	// IP networks contained in the list are considered.
	NetRestrict *netutil.Netlist `toml:",omitempty"`

	// NodeDatabase is the path to the database containing the previously seen
	// live nodes in the network.
	NodeDatabase string `toml:",omitempty"`

	// Protocols should contain the protocols supported
	// by the server. Matching protocols are launched for
	// each peer.
	Protocols []Protocol `toml:"-"`

	// If ListenAddr is set to a non-nil address, the server
	// will listen for incoming connections.
	//
	// If the port is zero, the operating system will pick a port. The
	// ListenAddr field will be updated with the actual address when
	// the server is started.
	ListenAddr string

	// If set to a non-nil value, the given NAT port mapper
	// is used to make the listening port available to the
	// Internet.
	NAT nat.Interface `toml:",omitempty"`

	// If Dialer is set to a non-nil value, the given Dialer
	// is used to dial outbound peer connections.
	Dialer NodeDialer `toml:"-"`

	// If NoDial is true, the server will not dial any peers.
	NoDial bool `toml:",omitempty"`

	// If EnableMsgEvents is set then the server will emit PeerEvents
	// whenever a message is sent to or received from a peer
	EnableMsgEvents bool
}

// Server manages all peer connections.
type Server struct {
	db                *sql.DB // MySQL database handle
	NeighborChan      chan []interface{}
	metaInfoChan      chan []interface{}
	p2pInfoChan       chan []interface{}
	EthInfoChan       chan []interface{}
	neighborInfoQueue *infoQueue
	metaInfoQueue     *infoQueue
	p2pInfoQueue      *infoQueue
	ethInfoQueue      *infoQueue
	pushTicker        *mticker.MutableTicker
	dialCheckTicker   *mticker.MutableTicker

	KnownNodeInfos *KnownNodeInfos // information on known nodes
	StrReplacer    *strings.Replacer
	dialstate      *dialstate

	// Config fields may not be modified while the server is running.
	Config

	// Hooks for testing. These are useful because we can inhibit
	// the whole protocol stack.
	newTransport func(net.Conn) transport
	newPeerHook  func(*Peer)

	lock    sync.Mutex // protects running
	running bool

	ntab         discoverTable
	udp          udp
	listener     net.Listener
	ourHandshake *protoHandshake
	lastLookup   time.Time
	DiscV5       *discv5.Network

	// These are for Peers, PeerCount (and nothing else).
	peerOp     chan peerOpFunc
	peerOpDone chan struct{}

	quit          chan struct{}
	addstatic     chan *discover.Node
	removestatic  chan *discover.Node
	posthandshake chan *conn
	addpeer       chan *conn
	delpeer       chan peerDrop
	loopWG        sync.WaitGroup // loop, listenLoop
	peerFeed      event.Feed
}

type udp interface {
	SetBlacklist(blacklist *netutil.Netlist)
}

type peerOpFunc func(map[discover.NodeID]*Peer)

type peerDrop struct {
	*Peer
	err       error
	requested bool // true if signaled by the peer
}

type connFlag int

const (
	dynDialedConn connFlag = 1 << iota
	staticDialedConn
	inboundConn
	trustedConn
)

// conn wraps a network connection with information gathered
// during the two handshakes.
type conn struct {
	fd net.Conn
	transport
	flags       connFlag
	cont        chan error      // The run loop uses cont to signal errors to SetupConn.
	id          discover.NodeID // valid after the encryption handshake
	version     uint64          // valid after the protocol handshake
	caps        []Cap           // valid after the protocol handshake
	name        string          // valid after the protocol handshake
	listenPort  uint16          // valid after the protocol handshake
	tcpPort     uint16          // valid after the protocol handshake
	connInfoCtx []interface{}
}

type transport interface {
	Rtt() float64
	// The two handshakes.
	doEncHandshake(prv *ecdsa.PrivateKey, dialDest *discover.Node) (discover.NodeID, error)
	doProtoHandshake(our *protoHandshake, connInfoCtx ...interface{}) (*protoHandshake, Msg, error)
	// The MsgReadWriter can only be used after the encryption
	// handshake has completed. The code uses conn.id to track this
	// by setting it to a non-nil value after the encryption handshake.
	MsgReadWriter
	// transports must provide Close because we use MsgPipe in some of
	// the tests. Closing the actual network connection doesn't do
	// anything in those tests because NsgPipe doesn't use it.
	close(err error)
}

func (c *conn) String() string {
	s := c.flags.String()
	if (c.id != discover.NodeID{}) {
		s += " " + c.id.String()
	}
	s += " " + c.fd.RemoteAddr().String()
	return s
}

func (f connFlag) String() string {
	s := ""
	if f&trustedConn != 0 {
		s += "-trusted"
	}
	if f&dynDialedConn != 0 {
		s += "-dyndial"
	}
	if f&staticDialedConn != 0 {
		s += "-staticdial"
	}
	if f&inboundConn != 0 {
		s += "-inbound"
	}
	if s != "" {
		s = s[1:]
	}
	return s
}

func (c *conn) is(f connFlag) bool {
	return c.flags&f != 0
}

func (c *conn) isInbound() bool {
	if c.flags&inboundConn != 0 || c.flags&trustedConn != 0 {
		return true
	}
	return false
}

// Peers returns all connected peers.
func (srv *Server) Peers() []*Peer {
	var ps []*Peer
	select {
	// Note: We'd love to put this function into a variable but
	// that seems to cause a weird compiler error in some
	// environments.
	case srv.peerOp <- func(peers map[discover.NodeID]*Peer) {
		for _, p := range peers {
			ps = append(ps, p)
		}
	}:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return ps
}

// PeerCount returns the number of connected peers.
func (srv *Server) PeerCount() int {
	var count int
	select {
	case srv.peerOp <- func(ps map[discover.NodeID]*Peer) { count = len(ps) }:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return count
}

// AddPeer connects to the given node and maintains the connection until the
// server is shut down. If the connection fails for any reason, the server will
// attempt to reconnect the peer.
func (srv *Server) AddPeer(node *discover.Node) {
	select {
	case srv.addstatic <- node:
	case <-srv.quit:
	}
}

// RemovePeer disconnects from the given node
func (srv *Server) RemovePeer(node *discover.Node) {
	select {
	case srv.removestatic <- node:
	case <-srv.quit:
	}
}

// SubscribePeers subscribes the given channel to peer events
func (srv *Server) SubscribeEvents(ch chan *PeerEvent) event.Subscription {
	return srv.peerFeed.Subscribe(ch)
}

// Self returns the local node's endpoint information.
func (srv *Server) Self() *discover.Node {
	srv.lock.Lock()
	defer srv.lock.Unlock()

	if !srv.running {
		return &discover.Node{IP: net.ParseIP("0.0.0.0")}
	}
	return srv.makeSelf(srv.listener, srv.ntab)
}

func (srv *Server) makeSelf(listener net.Listener, ntab discoverTable) *discover.Node {
	// If the server's not running, return an empty node.
	// If the node is running but discovery is off, manually assemble the node infos.
	if ntab == nil {
		// Inbound connections disabled, use zero address.
		if listener == nil {
			return &discover.Node{IP: net.ParseIP("0.0.0.0"), ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
		}
		// Otherwise inject the listener address too
		addr := listener.Addr().(*net.TCPAddr)
		return &discover.Node{
			ID:  discover.PubkeyID(&srv.PrivateKey.PublicKey),
			IP:  addr.IP,
			TCP: uint16(addr.Port),
		}
	}
	// Otherwise return the discovery node.
	return ntab.Self()
}

// Stop terminates the server and all active peer connections.
// It blocks until all active connections have been closed.
func (srv *Server) Stop() {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if !srv.running {
		return
	}
	srv.running = false
	close(srv.quit)
	if srv.listener != nil {
		// this unblocks listener Accept
		srv.listener.Close()
	}
	srv.loopWG.Wait()

	srv.closeSql()
}

// Start starts running the server.
// Servers can not be re-used after stopping.
func (srv *Server) Start() (err error) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("server already running")
	}
	if srv.Config.DialCheckFreq <= 0 {
		srv.Config.DialCheckFreq = 15
	}
	if srv.Config.PushFreq <= 0 {
		srv.Config.PushFreq = 1
	}

	srv.running = true
	log.Info("Starting P2P networking")

	// static fields
	if srv.PrivateKey == nil {
		return fmt.Errorf("Server.PrivateKey must be set to a non-nil key")
	}
	if srv.newTransport == nil {
		srv.newTransport = newRLPX
	}
	if srv.Dialer == nil {
		srv.Dialer = TCPDialer{&net.Dialer{Timeout: defaultDialTimeout}}
	}
	srv.quit = make(chan struct{})
	srv.addpeer = make(chan *conn)
	srv.delpeer = make(chan peerDrop)
	srv.posthandshake = make(chan *conn)
	srv.addstatic = make(chan *discover.Node)
	srv.removestatic = make(chan *discover.Node)
	srv.peerOp = make(chan peerOpFunc)
	srv.peerOpDone = make(chan struct{})

	// node-finder configurations
	// add bootnodes to static
	srv.StaticNodes = append(srv.StaticNodes, srv.BootstrapNodes...)
	// initialize knowNodeInfos
	srv.KnownNodeInfos = NewKnownNodeInfos()
	// initiate sql connection and prepare statements
	// from this point on, srv.closeSql() should be called when returning with error
	if err := srv.initSql(); err != nil {
		srv.closeSql()
		return err
	}
	// initiate string replacer
	srv.StrReplacer = strings.NewReplacer(
		"|", "",
		" ", "",
		"'", "",
		"\"", "")

	// node table
	if !srv.NoDiscovery {
		ntab, udp, err := discover.ListenUDP(srv.PrivateKey, srv.ListenAddr, srv.NAT, srv.NodeDatabase, srv.NetRestrict, srv.Blacklist, srv.NeighborChan)
		if err != nil {
			srv.closeSql()
			return err
		}
		if err := ntab.SetFallbackNodes(srv.BootstrapNodes); err != nil {
			srv.closeSql()
			return err
		}
		srv.ntab = ntab
		srv.udp = udp
	}

	dynPeers := (srv.MaxPeers + 1) / 2
	if srv.NoDiscovery {
		dynPeers = 0
	}
	dialer := newDialState(srv.StaticNodes, srv.ntab, dynPeers, srv.NetRestrict)
	dialer.blacklist = srv.Blacklist
	srv.dialstate = dialer
	srv.SetDialFreq(srv.DialFreq)

	// handshake
	srv.ourHandshake = &protoHandshake{Version: baseProtocolVersion, Name: srv.Name, ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
	for _, p := range srv.Protocols {
		srv.ourHandshake.Caps = append(srv.ourHandshake.Caps, p.cap())
	}
	// listen/dial
	if srv.ListenAddr != "" {
		if err := srv.startListening(); err != nil {
			srv.closeSql()
			return err
		}
	}
	if srv.NoDial && srv.ListenAddr == "" {
		log.Warn("P2P server will be useless, neither dialing nor listening")
	}

	srv.loopWG.Add(1)
	go srv.run(dialer)
	srv.running = true
	return nil
}

func (srv *Server) startListening() error {
	// Launch the TCP listener.
	listener, err := net.Listen("tcp", srv.ListenAddr)
	if err != nil {
		return err
	}
	laddr := listener.Addr().(*net.TCPAddr)
	srv.ListenAddr = laddr.String()
	srv.listener = listener
	srv.loopWG.Add(1)
	go srv.listenLoop()
	// Map the TCP listening port if NAT is configured.
	if !laddr.IP.IsLoopback() && srv.NAT != nil {
		srv.loopWG.Add(1)
		go func() {
			nat.Map(srv.NAT, srv.quit, "tcp", laddr.Port, laddr.Port, "ethereum p2p")
			srv.loopWG.Done()
		}()
	}
	return nil
}

type dialer interface {
	newTasks(running int, peers map[discover.NodeID]*Peer, now time.Time) ([]task, bool)
	newRedialTasks(peers map[discover.NodeID]*Peer, now time.Time) []task
	taskDone(task, time.Time)
	addStatic(*discover.Node)
	removeStatic(*discover.Node)
}

func (srv *Server) SetDialFreq(dialFreq int) {
	srv.DialFreq = dialFreq
	srv.dialstate.dialFreq = time.Duration(dialFreq) * time.Second
}

func (srv *Server) SetDialCheckFreq(dialCheckFreq int) {
	srv.DialCheckFreq = dialCheckFreq
	srv.dialCheckTicker.UpdateInterval(time.Duration(dialCheckFreq) * time.Second)
}

func (srv *Server) SetPushFreq(pushFreq int) {
	srv.PushFreq = pushFreq
	srv.pushTicker.UpdateInterval(time.Duration(pushFreq) * time.Second)
}

func (srv *Server) AddBlacklist(cidrs string) error {
	if srv.Blacklist == nil {
		if list, err := netutil.ParseNetlist(cidrs); err != nil {
			return err
		} else {
			srv.Blacklist = list
			srv.dialstate.blacklist = list
			srv.udp.SetBlacklist(list)
		}
	} else {
		ws := strings.NewReplacer(" ", "", "\n", "", "\t", "")
		masks := strings.Split(ws.Replace(cidrs), ",")
		for _, mask := range masks {
			if err := srv.Blacklist.AddNonDuplicate(mask); err != nil {
				return err
			}
		}
	}
	return nil
}

func (srv *Server) run(dialstate dialer) {
	defer srv.loopWG.Done()
	var (
		peers             = make(map[discover.NodeID]*Peer)
		trusted           = make(map[discover.NodeID]bool, len(srv.TrustedNodes))
		discoverdone      = make(chan task)
		dyndialdone       = make(chan task, srv.MaxDial)
		staticdialdone    = make(chan task)
		runningDiscover   []task
		runningDynDial    []task
		runningStaticDial []task
		queuedTasks       []task // tasks that can't run yet
	)
	// Put trusted nodes into a map to speed up checks.
	// Trusted peers are loaded on startup and cannot be
	// modified while the server is running.
	for _, n := range srv.TrustedNodes {
		trusted[n.ID] = true
	}

	// removes t from runningTasks
	delTask := func(t task) {
		log.Task("DONE", t.TaskInfoCtx())
		dialstate.taskDone(t, time.Now())
		switch t := t.(type) {
		case *dialTask:
			if t.flags&staticDialedConn != 0 {
				for i := range runningStaticDial {
					if runningStaticDial[i] == t {
						runningStaticDial = append(runningStaticDial[:i], runningStaticDial[i+1:]...)
						return
					}
				}
			} else {
				for i := range runningDynDial {
					if runningDynDial[i] == t {
						runningDynDial = append(runningDynDial[:i], runningDynDial[i+1:]...)
						return
					}
				}
			}
		case *discoverTask:
			// there is 1 running discoverTask at most or nothing
			runningDiscover = runningDiscover[:0]
			return
		default: // for testing
			for i := range runningDynDial {
				if runningDynDial[i] == t {
					runningDynDial = append(runningDynDial[:i], runningDynDial[i+1:]...)
					return
				}
			}
		}
	}
	// starts until max number of active tasks is satisfied
	startDynDialTasks := func(ts []task) (rest []task) {
		i := 0
		for ; len(runningDynDial) < srv.MaxDial && i < len(ts); i++ {
			t := ts[i]
			log.Task("NEW", t.TaskInfoCtx())
			go func() { t.Do(srv); dyndialdone <- t }()
			runningDynDial = append(runningDynDial, t)
		}
		return ts[i:]
	}
	scheduleDynDialTasks := func() {
		// Start from queue first.
		queuedTasks = append(queuedTasks[:0], startDynDialTasks(queuedTasks)...)
		// Query dialer for new tasks and start as many as possible now.
		if len(runningDynDial) < srv.MaxDial {
			nRunning := len(runningDynDial) + len(queuedTasks) + len(runningDiscover)
			nt, needDiscoverTask := dialstate.newTasks(nRunning, peers, time.Now())
			queuedTasks = append(queuedTasks, startDynDialTasks(nt)...)
			if needDiscoverTask {
				t := &discoverTask{}
				log.Task("NEW", t.TaskInfoCtx())
				go func() { t.Do(srv); discoverdone <- t }()
				runningDiscover = append(runningDiscover, t)
			}
		}
	}
	scheduleRedialTasks := func() {
		// Query dialer for new static tasks and start all
		for _, t := range dialstate.newRedialTasks(peers, time.Now()) {
			log.Task("NEW", t.TaskInfoCtx())
			go func(t task) { t.Do(srv); staticdialdone <- t }(t)
			runningStaticDial = append(runningStaticDial, t)
		}
	}

	// start redial check timer
	srv.dialCheckTicker = mticker.NewMutableTicker(time.Duration(srv.DialCheckFreq) * time.Second)
	// initial static dials
	scheduleRedialTasks()

running:
	for {
		scheduleDynDialTasks()

		select {
		case <-srv.quit:
			// The server was stopped. Run the cleanup logic.
			break running
		case <-srv.dialCheckTicker.C:
			scheduleRedialTasks()
		case n := <-srv.addstatic:
			// This channel is used by AddPeer to add to the
			// ephemeral static peer list. Add it to the dialer,
			// it will keep the node connected.
			log.Debug("Adding static node", "node", n)
			dialstate.addStatic(n)
		case n := <-srv.removestatic:
			// This channel is used by RemovePeer to send a
			// disconnect request to a peer and begin the
			// stop keeping the node connected
			log.Debug("Removing static node", "node", n)
			dialstate.removeStatic(n)
			if p, ok := peers[n.ID]; ok {
				p.Disconnect(DiscRequested)
			}
		case op := <-srv.peerOp:
			// This channel is used by Peers and PeerCount.
			op(peers)
			srv.peerOpDone <- struct{}{}
		case t := <-discoverdone:
			delTask(t)
		case t := <-dyndialdone:
			delTask(t)
		case t := <-staticdialdone:
			delTask(t)
		case c := <-srv.posthandshake:
			// A connection has passed the encryption handshake so
			// the remote identity is known (but hasn't been verified yet).
			if trusted[c.id] {
				// Ensure that the trusted flag is set before checking against MaxPeers.
				c.flags |= trustedConn
			}
			// TODO: track in-progress inbound node IDs (pre-Peer) to avoid dialing them.
			select {
			case c.cont <- srv.encHandshakeChecks(peers, c):
			case <-srv.quit:
				break running
			}
		case c := <-srv.addpeer:
			// At this point the connection is past the protocol handshake.
			// Its capabilities are known and the remote identity is verified.
			err := srv.protoHandshakeChecks(peers, c)
			if err == nil {
				// The handshakes are done and it passed all checks.
				p := newPeer(c, srv.Protocols)
				// If message events are enabled, pass the peerFeed
				// to the peer
				if srv.EnableMsgEvents {
					p.events = &srv.peerFeed
				}
				name := truncateName(c.name)
				log.Debug("Adding p2p peer", "id", c.id, "name", name, "addr", c.fd.RemoteAddr(), "peers", len(peers)+1)
				peers[c.id] = p
				go srv.runPeer(p)
			}
			// The dialer logic relies on the assumption that
			// dial tasks complete after the peer has been added or
			// discarded. Unblock the task last.
			select {
			case c.cont <- err:
			case <-srv.quit:
				break running
			}
		case pd := <-srv.delpeer:
			// A peer disconnected.
			// if sql db available, remotedRequested, and the discReason is TooManyPeers
			// update the node meta info's too many peer counter
			if pd.requested {
				if r, ok := pd.err.(DiscReason); ok && r == DiscTooManyPeers {
					id := pd.ID()
					var hash string
					nodeInfo := srv.KnownNodeInfos.GetInfo(id)
					if nodeInfo != nil {
						nodeInfo.RLock()
						hash = nodeInfo.Keccak256Hash
						nodeInfo.RUnlock()
					} else {
						hash = crypto.Keccak256Hash(id[:]).String()[2:]
					}
					if srv.metaInfoChan != nil {
						log.Sql("Queueing NodeMetaInfo", pd.ConnInfoCtx()...)
						srv.queueNodeMetaInfo(id, hash, false, false, true)
					}
				}
			}
			d := common.PrettyDuration(mclock.Now() - pd.created)
			log.Peer("REMOVE|DEVP2P", pd.ConnInfoCtx(), pd.Rtt(), time.Duration(d).Seconds())
			pd.log.Debug("Removing p2p peer", "duration", d, "peers", len(peers)-1, "req", pd.requested, "err", pd.err)
			delete(peers, pd.ID())
		}
	}
	srv.dialCheckTicker.Stop()

	log.Trace("P2P networking is spinning down")

	// Terminate discovery. If there is a running lookup it will terminate soon.
	if srv.ntab != nil {
		srv.ntab.Close()
	}
	// Disconnect all peers.
	for _, p := range peers {
		p.Disconnect(DiscQuitting)
	}

	// close node info channels
	if srv.metaInfoChan != nil {
		close(srv.metaInfoChan)
	}
	if srv.p2pInfoChan != nil {
		close(srv.p2pInfoChan)
	}
}

func (srv *Server) protoHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	// Drop connections with no matching protocols.
	if len(srv.Protocols) > 0 && countMatchingProtocols(srv.Protocols, c.caps) == 0 {
		return DiscUselessPeer
	}
	// Repeat the encryption handshake checks because the
	// peer set might have changed between the handshakes.
	return srv.encHandshakeChecks(peers, c)
}

func (srv *Server) encHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	switch {
	case !srv.NoMaxPeers && !c.is(trustedConn|staticDialedConn) && len(peers) >= srv.MaxPeers:
		return DiscTooManyPeers
	case peers[c.id] != nil:
		return DiscAlreadyConnected
	case c.id == srv.Self().ID:
		return DiscSelf
	default:
		return nil
	}
}

type tempError interface {
	Temporary() bool
}

// listenLoop runs in its own goroutine and accepts
// inbound connections.
func (srv *Server) listenLoop() {
	defer srv.loopWG.Done()
	log.Info("RLPx listener up", "self", srv.makeSelf(srv.listener, srv.ntab))

	// This channel acts as a semaphore limiting
	// active inbound connections that are lingering pre-handshake.
	// If all slots are taken, no further connections are accepted.
	tokens := maxAcceptConns
	if srv.MaxPendingPeers > 0 {
		tokens = srv.MaxPendingPeers
	}
	slots := make(chan struct{}, tokens)
	for i := 0; i < tokens; i++ {
		slots <- struct{}{}
	}

	for {
		// Wait for a handshake slot before accepting.
		<-slots

		var (
			fd  net.Conn
			err error
		)
		for {
			fd, err = srv.listener.Accept()
			if tempErr, ok := err.(tempError); ok && tempErr.Temporary() {
				log.Debug("Temporary read error", "err", err)
				continue
			} else if err != nil {
				log.Debug("Read error", "err", err)
				return
			}
			break
		}

		// Reject connections that do not match NetRestrict.
		if srv.NetRestrict != nil {
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok && !srv.NetRestrict.Contains(tcp.IP) {
				log.Debug("Rejected conn (not whitelisted in NetRestrict)", "addr", fd.RemoteAddr(), "transport", "tcp")
				fd.Close()
				slots <- struct{}{}
				continue
			}
		}

		// Reject connections that match Blacklist.
		if srv.Blacklist != nil {
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok && srv.Blacklist.Contains(tcp.IP) {
				log.Debug("Rejected conn (blacklisted)", "addr", fd.RemoteAddr(), "transport", "tcp")
				fd.Close()
				slots <- struct{}{}
				continue
			}
		}

		fd = newMeteredConn(fd, true)
		log.Trace("Accepted connection", "addr", fd.RemoteAddr())

		// Spawn the handler. It will give the slot back when the connection
		// has been established.
		go func() {
			srv.SetupConn(fd, inboundConn, nil)
			slots <- struct{}{}
		}()
	}
}

// SetupConn runs the handshakes and attempts to add the connection
// as a peer. It returns when the connection has been added as a peer
// or the handshakes have failed.
func (srv *Server) SetupConn(fd net.Conn, flags connFlag, dialDest *discover.Node) {
	// Prevent leftover pending conns from entering the handshake.
	srv.lock.Lock()
	running := srv.running
	srv.lock.Unlock()
	c := &conn{fd: fd, transport: srv.newTransport(fd), flags: flags, cont: make(chan error)}
	if !running {
		c.close(errServerStopped)
		return
	}
	// Run the encryption handshake.
	var err error
	if c.id, err = c.doEncHandshake(srv.PrivateKey, dialDest); err != nil {
		log.Trace("Failed RLPx handshake", "addr", c.fd.RemoteAddr(), "conn", c.flags, "err", err)
		c.close(err)
		return
	}
	c.connInfoCtx = []interface{}{
		"id", c.id.String(),
		"addr", c.fd.RemoteAddr().String(),
		"conn", c.flags.String(),
	}
	clog := log.New("id", c.id, "addr", c.fd.RemoteAddr(), "conn", c.flags)
	// For dialed connections, check that the remote public key matches.
	if dialDest != nil && c.id != dialDest.ID {
		c.close(DiscUnexpectedIdentity)
		clog.Trace("Dialed identity mismatch", "want", c, dialDest.ID)
		return
	}
	if err := srv.checkpoint(c, srv.posthandshake); err != nil {
		clog.Trace("Rejected peer before protocol handshake", "err", err)
		c.close(err)
		return
	}
	// Run the protocol handshake
	phs, msg, err := c.doProtoHandshake(srv.ourHandshake, c.connInfoCtx...)
	if err != nil {
		clog.Trace("Failed proto handshake", "err", err)
		if r, ok := err.(DiscReason); ok {
			log.DiscProto(msg.ReceivedAt, c.connInfoCtx, msg.PeerRtt, msg.PeerDuration, r.String())
			if r == DiscTooManyPeers {
				newInfo, dial, accept := srv.getNodeAddress(c, nil)
				if newInfo.TCPPort != 0 {
					srv.addNewStatic(c.id, newInfo)
				}
				if srv.metaInfoChan != nil {
					log.Sql("Queueing NodeMetaInfo", c.connInfoCtx...)
					srv.queueNodeMetaInfo(c.id, newInfo.Keccak256Hash, dial, accept, true)
				}
			}
		}
		c.close(err)
		return
	}
	if phs.ID != c.id {
		clog.Trace("Wrong devp2p handshake identity", "err", phs.ID)
		c.close(DiscUnexpectedIdentity)
		return
	}

	// update node information
	srv.storeNodeP2PInfo(c, &msg, phs)

	c.version, c.caps, c.name = phs.Version, phs.Caps, phs.Name
	if err := srv.checkpoint(c, srv.addpeer); err != nil {
		clog.Trace("Rejected peer", "err", err)
		c.close(err)
		return
	}
	// If the checks completed successfully, runPeer has now been
	// launched by run.
	log.Peer("ADD|DEVP2P", c.connInfoCtx, msg.PeerRtt, msg.PeerDuration)
}

func truncateName(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// checkpoint sends the conn to run, which performs the
// post-handshake checks for the stage (posthandshake, addpeer).
func (srv *Server) checkpoint(c *conn, stage chan<- *conn) error {
	select {
	case stage <- c:
	case <-srv.quit:
		return errServerStopped
	}
	select {
	case err := <-c.cont:
		return err
	case <-srv.quit:
		return errServerStopped
	}
}

// runPeer runs in its own goroutine for each peer.
// it waits until the Peer logic returns and removes
// the peer.
func (srv *Server) runPeer(p *Peer) {
	if srv.newPeerHook != nil {
		srv.newPeerHook(p)
	}

	// broadcast peer add
	srv.peerFeed.Send(&PeerEvent{
		Type: PeerEventTypeAdd,
		Peer: p.ID(),
	})

	// run the protocol
	remoteRequested, err := p.run()

	// broadcast peer drop
	srv.peerFeed.Send(&PeerEvent{
		Type:  PeerEventTypeDrop,
		Peer:  p.ID(),
		Error: err.Error(),
	})

	// Note: run waits for existing peers to be sent on srv.delpeer
	// before returning, so this send should not select on srv.quit.
	srv.delpeer <- peerDrop{p, err, remoteRequested}
}

// NodeInfo represents a short summary of the information known about the host.
type NodeInfo struct {
	ID    string `json:"id"`    // Unique node identifier (also the encryption key)
	Name  string `json:"name"`  // Name of the node, including client type, version, OS, custom data
	Enode string `json:"enode"` // Enode URL for adding this peer from remote peers
	IP    string `json:"ip"`    // IP address of the node
	Ports struct {
		Discovery int `json:"discovery"` // UDP listening port for discovery protocol
		Listener  int `json:"listener"`  // TCP listening port for RLPx
	} `json:"ports"`
	ListenAddr string                 `json:"listenAddr"`
	Protocols  map[string]interface{} `json:"protocols"`
}

// NodeInfo gathers and returns a collection of metadata known about the host.
func (srv *Server) NodeInfo() *NodeInfo {
	node := srv.Self()

	// Gather and assemble the generic node infos
	info := &NodeInfo{
		Name:       srv.Name,
		Enode:      node.String(),
		ID:         node.ID.String(),
		IP:         node.IP.String(),
		ListenAddr: srv.ListenAddr,
		Protocols:  make(map[string]interface{}),
	}
	info.Ports.Discovery = int(node.UDP)
	info.Ports.Listener = int(node.TCP)

	// Gather all the running protocol infos (only once per protocol type)
	for _, proto := range srv.Protocols {
		if _, ok := info.Protocols[proto.Name]; !ok {
			nodeInfo := interface{}("unknown")
			if query := proto.NodeInfo; query != nil {
				nodeInfo = proto.NodeInfo()
			}
			info.Protocols[proto.Name] = nodeInfo
		}
	}
	return info
}

// PeersInfo returns an array of metadata objects describing connected peers.
func (srv *Server) PeersInfo() []*PeerInfo {
	// Gather all the generic and sub-protocol specific infos
	infos := make([]*PeerInfo, 0, srv.PeerCount())
	for _, peer := range srv.Peers() {
		if peer != nil {
			infos = append(infos, peer.Info())
		}
	}
	// Sort the result array alphabetically by node identifier
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].ID > infos[j].ID {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	return infos
}
