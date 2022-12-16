package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ipfs/go-bitswap/client/internal"
	bsbpm "github.com/ipfs/go-bitswap/client/internal/blockpresencemanager"
	bsgetter "github.com/ipfs/go-bitswap/client/internal/getter"
	notifications "github.com/ipfs/go-bitswap/client/internal/notifications"
	bspm "github.com/ipfs/go-bitswap/client/internal/peermanager"
	bssim "github.com/ipfs/go-bitswap/client/internal/sessioninterestmanager"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	delay "github.com/ipfs/go-ipfs-delay"
	logging "github.com/ipfs/go-log"
	peer "github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	selfish "github.com/guillaumemichel/go-selfish-bitswap-client"
	"github.com/ipfs/go-bitswap/measurements"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
)

var log = logging.Logger("bs:sess")
var sflog = log.Desugar()

const (
	broadcastLiveWantsLimit = 64
)

// PeerManager keeps track of which sessions are interested in which peers
// and takes care of sending wants for the sessions
type PeerManager interface {
	// RegisterSession tells the PeerManager that the session is interested
	// in a peer's connection state
	RegisterSession(peer.ID, bspm.Session)
	// UnregisterSession tells the PeerManager that the session is no longer
	// interested in a peer's connection state
	UnregisterSession(uint64)
	// SendWants tells the PeerManager to send wants to the given peer
	SendWants(ctx context.Context, peerId peer.ID, wantBlocks []cid.Cid, wantHaves []cid.Cid)
	// BroadcastWantHaves sends want-haves to all connected peers (used for
	// session discovery)
	BroadcastWantHaves(context.Context, []cid.Cid)
	// SendCancels tells the PeerManager to send cancels to all peers
	SendCancels(context.Context, []cid.Cid)
}

// SessionManager manages all the sessions
type SessionManager interface {
	// Remove a session (called when the session shuts down)
	RemoveSession(sesid uint64)
	// Cancel wants (called when a call to GetBlocks() is cancelled)
	CancelSessionWants(sid uint64, wants []cid.Cid)
}

// SessionPeerManager keeps track of peers in the session
type SessionPeerManager interface {
	// PeersDiscovered indicates if any peers have been discovered yet
	PeersDiscovered() bool
	// Shutdown the SessionPeerManager
	Shutdown()
	// Adds a peer to the session, returning true if the peer is new
	AddPeer(peer.ID) bool
	// Removes a peer from the session, returning true if the peer existed
	RemovePeer(peer.ID) bool
	// All peers in the session
	Peers() []peer.ID
	// Whether there are any peers in the session
	HasPeers() bool
	// Protect connection from being pruned by the connection manager
	ProtectConnection(peer.ID)
}

// ProviderFinder is used to find providers for a given key
type ProviderFinder interface {
	// FindProvidersAsync searches for peers that provide the given CID
	FindProvidersAsync(ctx context.Context, k cid.Cid) <-chan peer.AddrInfo
}

// opType is the kind of operation that is being processed by the event loop
type opType int

const (
	// Receive blocks
	opReceive opType = iota
	// Want blocks
	opWant
	// Cancel wants
	opCancel
	// Broadcast want-haves
	opBroadcast
	// Wants sent to peers
	opWantsSent
)

type op struct {
	from peer.ID
	op   opType
	keys []cid.Cid
}

// Session holds state for an individual bitswap transfer operation.
// This allows bitswap to make smarter decisions about who to send wantlist
// info to, and who to request blocks from.
type Session struct {
	// dependencies
	ctx            context.Context
	shutdown       func()
	sm             SessionManager
	pm             PeerManager
	sprm           SessionPeerManager
	providerFinder ProviderFinder
	sim            *bssim.SessionInterestManager

	sw  sessionWants
	sws sessionWantSender

	latencyTrkr latencyTracker

	// channels
	incoming      chan op
	tickDelayReqs chan time.Duration

	// do not touch outside run loop
	idleTick            *time.Timer
	periodicSearchTimer *time.Timer
	baseTickDelay       time.Duration
	consecutiveTicks    int
	initialSearchDelay  time.Duration
	periodicSearchDelay delay.D
	// identifiers
	notif notifications.PubSub
	id    uint64

	self peer.ID
	host host.Host

	bitswapOnly bool
	dhtFound    bool

	mutex *sync.Mutex

	cid       cid.Cid
	startTime time.Time
	ttfb      int // time in ms
	providers []peer.ID
}

// New creates a new bitswap session whose lifetime is bounded by the
// given context.
func New(
	ctx context.Context,
	sm SessionManager,
	id uint64,
	sprm SessionPeerManager,
	providerFinder ProviderFinder,
	sim *bssim.SessionInterestManager,
	pm PeerManager,
	bpm *bsbpm.BlockPresenceManager,
	notif notifications.PubSub,
	initialSearchDelay time.Duration,
	periodicSearchDelay delay.D,
	host host.Host) *Session {

	ctx, cancel := context.WithCancel(ctx)
	s := &Session{
		sw:                  newSessionWants(broadcastLiveWantsLimit),
		tickDelayReqs:       make(chan time.Duration),
		ctx:                 ctx,
		shutdown:            cancel,
		sm:                  sm,
		pm:                  pm,
		sprm:                sprm,
		providerFinder:      providerFinder,
		sim:                 sim,
		incoming:            make(chan op, 128),
		latencyTrkr:         latencyTracker{},
		notif:               notif,
		baseTickDelay:       time.Millisecond * 500,
		id:                  id,
		initialSearchDelay:  initialSearchDelay,
		periodicSearchDelay: periodicSearchDelay,
		self:                host.ID(),
		host:                host,
		bitswapOnly:         false,
		dhtFound:            false,
		mutex:               &sync.Mutex{},
		startTime:           time.Now(),
		ttfb:                0,
		providers:           make([]peer.ID, 0),
	}
	s.sws = newSessionWantSender(id, pm, sprm, sm, bpm, s.onWantsSent, s.onPeersExhausted)

	go s.run(ctx)

	return s
}

func (s *Session) ID() uint64 {
	return s.id
}

func (s *Session) Shutdown() {
	s.shutdown()
}

// ReceiveFrom receives incoming blocks from the given peer.
func (s *Session) ReceiveFrom(from peer.ID, ks []cid.Cid, haves []cid.Cid, dontHaves []cid.Cid) {
	// The SessionManager tells each Session about all keys that it may be
	// interested in. Here the Session filters the keys to the ones that this
	// particular Session is interested in.
	interestedRes := s.sim.FilterSessionInterested(s.id, ks, haves, dontHaves)
	ks = interestedRes[0]
	haves = interestedRes[1]
	dontHaves = interestedRes[2]
	s.logReceiveFrom(from, ks, haves, dontHaves)

	// Inform the session want sender that a message has been received
	s.sws.Update(from, ks, haves, dontHaves)

	if len(ks) == 0 {
		return
	}

	// Inform the session that blocks have been received
	select {
	case s.incoming <- op{op: opReceive, keys: ks, from: from}:
	case <-s.ctx.Done():
	}
}

func (s *Session) logReceiveFrom(from peer.ID, interestedKs []cid.Cid, haves []cid.Cid, dontHaves []cid.Cid) {
	// Save some CPU cycles if log level is higher than debug
	timestamp := time.Now().Format("2006-01-02T15:04:05.999Z") + ","
	peerid := "," + from.String()

	if ce := sflog.Check(zap.DebugLevel, "Bitswap <- rcv message"); ce == nil {
		//return
	}

	for _, c := range interestedKs {
		measurements.LogStringToFile(timestamp+"BLOCK,"+c.String()+peerid, measurements.PACKETSLOGS)
		log.Debugw("Bitswap <- block", "local", s.self, "from", from, "cid", c, "session", s.id)
	}
	for _, c := range haves {
		measurements.LogStringToFile(timestamp+"HAVE,"+c.String()+peerid, measurements.PACKETSLOGS)
		log.Debugw("Bitswap <- HAVE", "local", s.self, "from", from, "cid", c, "session", s.id)
	}
	for _, c := range dontHaves {
		measurements.LogStringToFile(timestamp+"DONT_HAVE,"+c.String()+peerid, measurements.PACKETSLOGS)
		log.Debugw("Bitswap <- DONT_HAVE", "local", s.self, "from", from, "cid", c, "session", s.id)
	}
}

// GetBlock fetches a single block.
func (s *Session) GetBlock(ctx context.Context, k cid.Cid) (blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlock")
	// get from the context if the request is bitswap only
	bitswapOnly := ctx.Value("bitswapOnly")
	if bitswapOnly != nil {
		if b, ok := bitswapOnly.(bool); ok {
			// if it is bitswap only, the session will be set as bitswap only
			s.bitswapOnly = b
		}
	}

	s.cid = k

	defer span.End()
	return bsgetter.SyncGetBlock(ctx, k, s.GetBlocks)
}

// GetBlocks fetches a set of blocks within the context of this session and
// returns a channel that found blocks will be returned on. No order is
// guaranteed on the returned blocks.
func (s *Session) GetBlocks(ctx context.Context, keys []cid.Cid) (<-chan blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlocks")
	defer span.End()

	return bsgetter.AsyncGetBlocks(ctx, s.ctx, keys, s.notif,
		func(ctx context.Context, keys []cid.Cid) {
			select {
			case s.incoming <- op{op: opWant, keys: keys}:
			case <-ctx.Done():
			case <-s.ctx.Done():
			}
		},
		func(keys []cid.Cid) {
			select {
			case s.incoming <- op{op: opCancel, keys: keys}:
			case <-s.ctx.Done():
			}
		},
	)
}

// SetBaseTickDelay changes the rate at which ticks happen.
func (s *Session) SetBaseTickDelay(baseTickDelay time.Duration) {
	select {
	case s.tickDelayReqs <- baseTickDelay:
	case <-s.ctx.Done():
	}
}

// onWantsSent is called when wants are sent to a peer by the session wants sender
func (s *Session) onWantsSent(p peer.ID, wantBlocks []cid.Cid, wantHaves []cid.Cid) {
	allBlks := append(wantBlocks[:len(wantBlocks):len(wantBlocks)], wantHaves...)
	s.nonBlockingEnqueue(op{op: opWantsSent, keys: allBlks})
}

// onPeersExhausted is called when all available peers have sent DONT_HAVE for
// a set of cids (or all peers become unavailable)
func (s *Session) onPeersExhausted(ks []cid.Cid) {
	s.nonBlockingEnqueue(op{op: opBroadcast, keys: ks})
}

// We don't want to block the sessionWantSender if the incoming channel
// is full. So if we can't immediately send on the incoming channel spin
// it off into a go-routine.
func (s *Session) nonBlockingEnqueue(o op) {
	select {
	case s.incoming <- o:
	default:
		go func() {
			select {
			case s.incoming <- o:
			case <-s.ctx.Done():
			}
		}()
	}
}

// Session run loop -- everything in this function should not be called
// outside of this loop
func (s *Session) run(ctx context.Context) {
	go s.sws.Run()

	s.idleTick = time.NewTimer(s.initialSearchDelay)
	s.periodicSearchTimer = time.NewTimer(s.periodicSearchDelay.NextWaitTime())
	deadlineTimer := time.NewTimer(measurements.DISCOVERY_TIMEOUT)
	for {
		select {
		case oper := <-s.incoming:
			switch oper.op {
			case opReceive:
				// Received blocks
				s.handleReceive(oper.keys, oper.from)
			case opWant:
				// Client wants blocks
				s.wantBlocks(ctx, oper.keys)
			case opCancel:
				// Wants were cancelled
				s.sw.CancelPending(oper.keys)
				s.sws.Cancel(oper.keys)
			case opWantsSent:
				// Wants were sent to a peer
				s.sw.WantsSent(oper.keys)
			case opBroadcast:
				// Broadcast want-haves to all peers
				s.broadcast(ctx, oper.keys)
			default:
				panic("unhandled operation")
			}
		case <-s.idleTick.C:
			// The session hasn't received blocks for a while, broadcast
			s.broadcast(ctx, nil)
		case <-s.periodicSearchTimer.C:
			// Periodically search for a random live want
			s.handlePeriodicSearch(ctx)
		case baseTickDelay := <-s.tickDelayReqs:
			// Set the base tick delay
			s.baseTickDelay = baseTickDelay
		case <-ctx.Done():
			// Shutdown
			s.handleShutdown()
			return
		case <-deadlineTimer.C:
			// bitswap timer expired, cancel request and start dht lookup
			if s.bitswapOnly && len(s.providers) == 0 {
				s.timesup(ctx)
			}
		}
	}
}

func (s *Session) timesup(ctx context.Context) {
	s.sw.CancelPending([]cid.Cid{s.cid})

	// set timer for the dht lookup
	dhtTimer := time.NewTimer(measurements.DHT_TIMEOUT)
	// start the dht lookup
	// the request has an internal 10s timeout

	found := make(chan bool)
	for prov := range s.providerFinder.FindProvidersAsync(ctx, s.cid) {
		s.dhtFound = true
		go func(prov peer.AddrInfo) {
			// create a new selfish bitswap session with the discovered provider
			selfishp2p, err := libp2p.New()
			if err != nil {
				fmt.Println("couldn't create selfishp2p", err)
			}
			// add provider peerid and multiaddresses to the new node
			selfishp2p.Peerstore().AddAddrs(prov.ID, prov.Addrs, time.Hour)

			ssession := selfish.New(selfishp2p, prov.ID)
			// try to get the block from the selfish session
			_, err = ssession.Get(s.cid)
			if err == nil {
				// if we were able to get the block, add the provider
				// if the provider isn't already in the provider list
				alreadyProv := false
				for _, p := range s.providers {
					if p == prov.ID {
						alreadyProv = true
					}
				}
				if !alreadyProv {
					s.mutex.Lock()
					s.providers = append(s.providers, prov.ID)
					s.mutex.Unlock()
				}

				found <- true
			}
		}(prov)
	}

	if s.dhtFound {
		select {
		case <-found:
		case <-dhtTimer.C:
		}
	}

	// stop the current bitswap requests
	s.shutdown()
}

// logResults
// format: starttime, cid, #peerset, "NO_PROV" OR "FETCH_FAILED" OR (ttfb +) providers_peerids
// (ttfb) provided if bitswap successfully discovered the block
// note that the ttfb variable is currently the time to fetch the block (time to last bit)
func (s *Session) logResults() {
	str := s.startTime.Format("2006-01-02T15:04:05.999Z") + "," +
		s.cid.String() + "," + fmt.Sprint(len(s.host.Network().Peers()))

	if s.dhtFound {
		// the dht lookup returned peerids
		if len(s.providers) > 0 {
			// selfish bitswap was able to fetch the block from at least one of these peers
			// log the first provider
			for _, p := range s.providers {
				str += "," + p.String()
			}
		} else {
			// selfish bitswap couldn't fetch the block from any providers
			str += "," + measurements.FETCH_FAILED
		}
	} else if s.ttfb > 0 {
		// bitswap successfully found the block within the DISCOVERY_TIMEOUT
		// log ttfb and all successful providers
		str += "," + fmt.Sprint(s.ttfb)
		for _, p := range s.providers {
			str += "," + p.String()
		}
	} else {
		// the dht lookup didn't return any providers for the cid
		str += "," + measurements.NO_PROVS
	}

	measurements.LogStringToFile(str, measurements.LOGFILE)
}

// Called when the session hasn't received any blocks for some time, or when
// all peers in the session have sent DONT_HAVE for a particular set of CIDs.
// Send want-haves to all connected peers, and search for new peers with the CID.
func (s *Session) broadcast(ctx context.Context, wants []cid.Cid) {
	// If this broadcast is because of an idle timeout (we haven't received
	// any blocks for a while) then broadcast all pending wants
	if wants == nil {
		wants = s.sw.PrepareBroadcast()
	}

	// Broadcast a want-have for the live wants to everyone we're connected to
	s.broadcastWantHaves(ctx, wants)

	// do not find providers on consecutive ticks
	// -- just rely on periodic search widening
	if len(wants) > 0 && (s.consecutiveTicks == 0) {
		// Search for providers who have the first want in the list.
		// Typically if the provider has the first block they will have
		// the rest of the blocks also.
		log.Debugw("FindMorePeers", "session", s.id, "cid", wants[0], "pending", len(wants))
		s.findMorePeers(ctx, wants[0])
	}
	s.resetIdleTick()

	// If we have live wants record a consecutive tick
	if s.sw.HasLiveWants() {
		s.consecutiveTicks++
	}
}

// handlePeriodicSearch is called periodically to search for providers of a
// randomly chosen CID in the sesssion.
func (s *Session) handlePeriodicSearch(ctx context.Context) {
	randomWant := s.sw.RandomLiveWant()
	if !randomWant.Defined() {
		return
	}

	// TODO: come up with a better strategy for determining when to search
	// for new providers for blocks.
	s.findMorePeers(ctx, randomWant)

	s.broadcastWantHaves(ctx, []cid.Cid{randomWant})

	s.periodicSearchTimer.Reset(s.periodicSearchDelay.NextWaitTime())
}

// findMorePeers attempts to find more peers for a session by searching for
// providers for the given Cid
func (s *Session) findMorePeers(ctx context.Context, c cid.Cid) {
	// Bitswap only mode, don't request new providers from the DHT
	if !s.bitswapOnly {
		go func(k cid.Cid) {
			for p := range s.providerFinder.FindProvidersAsync(ctx, k) {
				// When a provider indicates that it has a cid, it's equivalent to
				// the providing peer sending a HAVE
				s.sws.Update(p.ID, nil, []cid.Cid{c}, nil)
			}
		}(c)
	}
}

// handleShutdown is called when the session shuts down
func (s *Session) handleShutdown() {
	// Log bitswap measurements results
	s.logResults()
	// Stop the idle timer
	s.idleTick.Stop()
	// Shut down the session peer manager
	s.sprm.Shutdown()
	// Shut down the sessionWantSender (blocks until sessionWantSender stops
	// sending)
	s.sws.Shutdown()
	// Signal to the SessionManager that the session has been shutdown
	// and can be cleaned up
	s.sm.RemoveSession(s.id)
}

// handleReceive is called when the session receives blocks from a peer
func (s *Session) handleReceive(ks []cid.Cid, from peer.ID) {
	now := time.Now()
	// Record which blocks have been received and figure out the total latency
	// for fetching the blocks
	wanted, totalLatency := s.sw.BlocksReceived(ks)
	if len(wanted) == 0 {
		return
	}
	// verify if we received the target cid
	// ideally we should only get this one
	for _, k := range wanted {
		if k == s.cid {
			// record the provider peerid is not already in providers
			alreadyProv := false
			for _, p := range s.providers {
				if p == from {
					alreadyProv = true
				}
			}
			if !alreadyProv {
				s.mutex.Lock()
				s.providers = append(s.providers, from)
				s.mutex.Unlock()
			}
			if s.ttfb == 0 {
				// record the latency only if no latency was set before, we want to smallest latency
				s.ttfb = int(now.Sub(s.startTime).Milliseconds())
			}
		}
	}

	// Record latency
	s.latencyTrkr.receiveUpdate(len(wanted), totalLatency)

	// Inform the SessionInterestManager that this session is no longer
	// expecting to receive the wanted keys
	s.sim.RemoveSessionWants(s.id, wanted)

	s.idleTick.Stop()

	// We've received new wanted blocks, so reset the number of ticks
	// that have occurred since the last new block
	s.consecutiveTicks = 0

	s.resetIdleTick()
}

// wantBlocks is called when blocks are requested by the client
func (s *Session) wantBlocks(ctx context.Context, newks []cid.Cid) {
	if len(newks) > 0 {
		// Inform the SessionInterestManager that this session is interested in the keys
		s.sim.RecordSessionInterest(s.id, newks)
		// Tell the sessionWants tracker that that the wants have been requested
		s.sw.BlocksRequested(newks)
		// Tell the sessionWantSender that the blocks have been requested
		s.sws.Add(newks)
	}

	// If we have discovered peers already, the sessionWantSender will
	// send wants to them
	if s.sprm.PeersDiscovered() {
		return
	}

	// No peers discovered yet, broadcast some want-haves
	ks := s.sw.GetNextWants()
	if len(ks) > 0 {
		log.Infow("No peers - broadcasting", "session", s.id, "want-count", len(ks))
		s.broadcastWantHaves(ctx, ks)
	}
}

// Send want-haves to all connected peers
func (s *Session) broadcastWantHaves(ctx context.Context, wants []cid.Cid) {
	log.Debugw("broadcastWantHaves", "session", s.id, "cids", wants)
	s.pm.BroadcastWantHaves(ctx, wants)
}

// The session will broadcast if it has outstanding wants and doesn't receive
// any blocks for some time.
// The length of time is calculated
//   - initially
//     as a fixed delay
//   - once some blocks are received
//     from a base delay and average latency, with a backoff
func (s *Session) resetIdleTick() {
	var tickDelay time.Duration
	if !s.latencyTrkr.hasLatency() {
		tickDelay = s.initialSearchDelay
	} else {
		avLat := s.latencyTrkr.averageLatency()
		tickDelay = s.baseTickDelay + (3 * avLat)
	}
	tickDelay = tickDelay * time.Duration(1+s.consecutiveTicks)
	s.idleTick.Reset(tickDelay)
}

// latencyTracker keeps track of the average latency between sending a want
// and receiving the corresponding block
type latencyTracker struct {
	totalLatency time.Duration
	count        int
}

func (lt *latencyTracker) hasLatency() bool {
	return lt.totalLatency > 0 && lt.count > 0
}

func (lt *latencyTracker) averageLatency() time.Duration {
	return lt.totalLatency / time.Duration(lt.count)
}

func (lt *latencyTracker) receiveUpdate(count int, totalLatency time.Duration) {
	lt.totalLatency += totalLatency
	lt.count += count
}
