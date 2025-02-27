// +build linux

package netlink

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/agent-payload/process"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	ct "github.com/florianl/go-conntrack"
	"github.com/pkg/errors"
)

const (
	initializationTimeout = time.Second * 10

	compactInterval = time.Minute * 3

	// generationLength must be greater than compactInterval to ensure we have  multiple compactions per generation
	generationLength = compactInterval + time.Minute
)

// Conntracker is a wrapper around go-conntracker that keeps a record of all connections in user space
type Conntracker interface {
	GetTranslationForConn(ip util.Address, port uint16, transport process.ConnectionType) *IPTranslation
	ClearShortLived()
	GetStats() map[string]int64
	Close()
}

type connKey struct {
	ip   util.Address
	port uint16

	// the transport protocol of the connection, using the same values as specified in the agent payload.
	transport process.ConnectionType
}

type connValue struct {
	*IPTranslation
	expGeneration uint8
}

type realConntracker struct {
	sync.Mutex

	// we need two nfct handles because we can only register one callback per connection at a time
	nfct    *ct.Nfct
	nfctDel *ct.Nfct

	state map[connKey]*connValue

	// a short term buffer of connections to IPTranslations. Since we cannot make sure that tracer.go
	// will try to read the translation for an IP before the delete callback happens, we will
	// safe a fixed number of connections
	shortLivedBuffer map[connKey]*IPTranslation

	// the maximum size of the short lived buffer
	maxShortLivedBuffer int

	// The maximum size the state map will grow before we reject new entries
	maxStateSize int

	statsTicker   *time.Ticker
	compactTicker *time.Ticker
	stats         struct {
		gets                 int64
		getTimeTotal         int64
		registers            int64
		registersTotalTime   int64
		unregisters          int64
		unregistersTotalTime int64
		expiresTotal         int64
	}
	exceededSizeLogLimit *util.LogLimit
}

// NewConntracker creates a new conntracker with a short term buffer capped at the given size
func NewConntracker(procRoot string, deleteBufferSize, maxStateSize int) (Conntracker, error) {
	var (
		err         error
		conntracker Conntracker
	)

	done := make(chan struct{})

	go func() {
		conntracker, err = newConntrackerOnce(procRoot, deleteBufferSize, maxStateSize)
		done <- struct{}{}
	}()

	select {
	case <-done:
		return conntracker, err
	case <-time.After(initializationTimeout):
		return nil, fmt.Errorf("could not initialize conntrack after: %s", initializationTimeout)
	}
}

func newConntrackerOnce(procRoot string, deleteBufferSize, maxStateSize int) (Conntracker, error) {
	if deleteBufferSize <= 0 {
		return nil, fmt.Errorf("short term buffer size is less than 0")
	}

	netns := getGlobalNetNSFD(procRoot)

	logger := getLogger()
	nfct, err := ct.Open(&ct.Config{ReadTimeout: 10 * time.Millisecond, NetNS: netns, Logger: logger})
	if err != nil {
		return nil, err
	}

	nfctDel, err := ct.Open(&ct.Config{ReadTimeout: 10 * time.Millisecond, NetNS: netns, Logger: logger})
	if err != nil {
		return nil, errors.Wrap(err, "failed to open delete NFCT")
	}

	ctr := &realConntracker{
		nfct:                 nfct,
		nfctDel:              nfctDel,
		compactTicker:        time.NewTicker(compactInterval),
		state:                make(map[connKey]*connValue),
		shortLivedBuffer:     make(map[connKey]*IPTranslation),
		maxShortLivedBuffer:  deleteBufferSize,
		maxStateSize:         maxStateSize,
		exceededSizeLogLimit: util.NewLogLimit(10, time.Minute*10),
	}

	// seed the state
	sessions, err := nfct.Dump(ct.Conntrack, ct.IPv4)
	if err != nil {
		return nil, err
	}
	ctr.loadInitialState(sessions)
	log.Debugf("seeded IPv4 state")

	sessions, err = nfct.Dump(ct.Conntrack, ct.IPv6)
	if err != nil {
		// this is not fatal because we've already seeded with IPv4
		log.Errorf("Failed to dump IPv6")
	}
	ctr.loadInitialState(sessions)
	log.Debugf("seeded IPv6 state")

	go ctr.run()

	nfct.Register(context.Background(), ct.Conntrack, ct.NetlinkCtNew|ct.NetlinkCtExpectedNew|ct.NetlinkCtUpdate, ctr.register)
	log.Debugf("initialized register hook")

	nfctDel.Register(context.Background(), ct.Conntrack, ct.NetlinkCtDestroy, ctr.unregister)
	log.Debugf("initialized unregister hook")

	log.Infof("initialized conntrack")

	return ctr, nil
}

func (ctr *realConntracker) GetTranslationForConn(ip util.Address, port uint16, transport process.ConnectionType) *IPTranslation {
	then := time.Now().UnixNano()

	ctr.Lock()
	defer ctr.Unlock()

	k := connKey{ip, port, transport}
	var result *IPTranslation
	value, ok := ctr.state[k]
	if !ok {
		result = ctr.shortLivedBuffer[k]
	} else {
		value.expGeneration = getNthGeneration(generationLength, then, 3)
		result = value.IPTranslation
	}

	now := time.Now().UnixNano()
	atomic.AddInt64(&ctr.stats.gets, 1)
	atomic.AddInt64(&ctr.stats.getTimeTotal, now-then)
	return result
}

func (ctr *realConntracker) ClearShortLived() {
	ctr.Lock()
	defer ctr.Unlock()

	ctr.shortLivedBuffer = make(map[connKey]*IPTranslation, len(ctr.shortLivedBuffer))
}

func (ctr *realConntracker) GetStats() map[string]int64 {
	// only a few stats are locked
	ctr.Lock()
	size := len(ctr.state)
	stBufSize := len(ctr.shortLivedBuffer)
	ctr.Unlock()

	m := map[string]int64{
		"state_size":             int64(size),
		"short_term_buffer_size": int64(stBufSize),
		"expires":                int64(ctr.stats.expiresTotal),
	}

	if ctr.stats.gets != 0 {
		m["gets_total"] = ctr.stats.gets
		m["nanoseconds_per_get"] = ctr.stats.getTimeTotal / ctr.stats.gets
	}
	if ctr.stats.registers != 0 {
		m["registers_total"] = ctr.stats.registers
		m["nanoseconds_per_register"] = ctr.stats.registersTotalTime / ctr.stats.registers
	}
	if ctr.stats.unregisters != 0 {
		m["unregisters_total"] = ctr.stats.unregisters
		m["nanoseconds_per_unregister"] = ctr.stats.unregistersTotalTime / ctr.stats.unregisters

	}

	return m
}

func (ctr *realConntracker) Close() {
	ctr.compactTicker.Stop()
	ctr.exceededSizeLogLimit.Close()
}

func (ctr *realConntracker) loadInitialState(sessions []ct.Con) {
	gen := getNthGeneration(generationLength, time.Now().UnixNano(), 3)
	for _, c := range sessions {
		if isNAT(c) {
			if k, ok := formatKey(c); ok {
				ctr.state[k] = formatIPTranslation(c, gen)
			}
		}
	}
}

// register is registered to be called whenever a conntrack update/create is called.
// it will keep being called until it returns nonzero.
func (ctr *realConntracker) register(c ct.Con) int {
	// don't bother storing if the connection is not NAT
	if !isNAT(c) {
		return 0
	}

	key, ok := formatKey(c)
	if !ok {
		return 0
	}

	now := time.Now().UnixNano()
	ctr.Lock()
	defer ctr.Unlock()

	if len(ctr.state) >= ctr.maxStateSize {
		ctr.logExceededSize()
		return 0
	}

	generation := getNthGeneration(generationLength, now, 3)
	ctr.state[key] = formatIPTranslation(c, generation)

	then := time.Now().UnixNano()
	atomic.AddInt64(&ctr.stats.registers, 1)
	atomic.AddInt64(&ctr.stats.registersTotalTime, then-now)

	return 0
}

func (ctr *realConntracker) logExceededSize() {
	if ctr.exceededSizeLogLimit.ShouldLog() {
		log.Warnf("exceeded maximum conntrack state size: %d entries. You may need to increase system_probe_config.max_tracked_connections (will log first ten times, and then once every 10 minutes)", ctr.maxStateSize)
	}
}

// unregister is registered to be called whenever a conntrack entry is destroyed.
// it will keep being called until it returns nonzero.
func (ctr *realConntracker) unregister(c ct.Con) int {
	if !isNAT(c) {
		return 0
	}

	key, ok := formatKey(c)
	if !ok {
		return 0
	}

	now := time.Now().UnixNano()

	ctr.Lock()
	defer ctr.Unlock()

	// move the mapping from the permanent to "short lived" connection
	translation, ok := ctr.state[key]

	delete(ctr.state, key)
	if len(ctr.shortLivedBuffer) < ctr.maxShortLivedBuffer && ok {
		ctr.shortLivedBuffer[key] = translation.IPTranslation
	} else {
		log.Warn("exceeded maximum tracked short lived connections")
	}

	then := time.Now().UnixNano()
	atomic.AddInt64(&ctr.stats.unregisters, 1)
	atomic.AddInt64(&ctr.stats.unregistersTotalTime, then-now)

	return 0
}

func (ctr *realConntracker) run() {
	for range ctr.compactTicker.C {
		ctr.compact()
	}
}

func (ctr *realConntracker) compact() {
	ctr.Lock()
	defer ctr.Unlock()

	gen := getCurrentGeneration(generationLength, time.Now().UnixNano())

	// https://github.com/golang/go/issues/20135
	copied := make(map[connKey]*connValue, len(ctr.state))
	for k, v := range ctr.state {
		if v.expGeneration != gen {
			copied[k] = v
		} else {
			atomic.AddInt64(&ctr.stats.expiresTotal, 1)
		}
	}
	ctr.state = copied
}

func isNAT(c ct.Con) bool {
	if c.Origin == nil ||
		c.Reply == nil ||
		c.Origin.Proto == nil ||
		c.Reply.Proto == nil ||
		c.Origin.Proto.SrcPort == nil ||
		c.Origin.Proto.DstPort == nil ||
		c.Reply.Proto.SrcPort == nil ||
		c.Reply.Proto.DstPort == nil {
		return false
	}

	return !(*c.Origin.Src).Equal(*c.Reply.Dst) ||
		!(*c.Origin.Dst).Equal(*c.Reply.Src) ||
		*c.Origin.Proto.SrcPort != *c.Reply.Proto.DstPort ||
		*c.Origin.Proto.DstPort != *c.Reply.Proto.SrcPort
}

// ReplSrcIP extracts the source IP of the reply tuple from a conntrack entry
func ReplSrcIP(c ct.Con) net.IP {
	return *c.Reply.Src
}

// ReplDstIP extracts the dest IP of the reply tuple from a conntrack entry
func ReplDstIP(c ct.Con) net.IP {
	return *c.Reply.Dst
}

func formatIPTranslation(c ct.Con, generation uint8) *connValue {
	replSrcIP := ReplSrcIP(c)
	replDstIP := ReplDstIP(c)

	replSrcPort := *c.Reply.Proto.SrcPort
	replDstPort := *c.Reply.Proto.DstPort

	return &connValue{
		IPTranslation: &IPTranslation{
			ReplSrcIP:   util.AddressFromNetIP(replSrcIP),
			ReplDstIP:   util.AddressFromNetIP(replDstIP),
			ReplSrcPort: replSrcPort,
			ReplDstPort: replDstPort,
		},
		expGeneration: generation,
	}
}

func formatKey(c ct.Con) (k connKey, ok bool) {
	ok = true
	k.ip = util.AddressFromNetIP(*c.Origin.Src)
	k.port = *c.Origin.Proto.SrcPort

	proto := *c.Origin.Proto.Number
	switch proto {
	case 6:
		k.transport = process.ConnectionType_tcp
	case 17:
		k.transport = process.ConnectionType_udp

	default:
		ok = false
	}

	return
}
