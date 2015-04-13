package nameserver

import (
	"container/heap"
	"errors"
	"github.com/miekg/dns"
	. "github.com/weaveworks/weave/common"
	"math"
	"math/rand"
	"sync"
	"time"
)

var (
	errInvalidCapacity = errors.New("Invalid cache capacity")
	errCouldNotResolve = errors.New("Could not resolve")
	errTimeout         = errors.New("Timeout while waiting for resolution")
	errNoLocalReplies  = errors.New("No local replies")
)

const (
	defPendingTimeout int = 5 // timeout for a resolution
)

const nullTTL = 0	// a null TTL

type entryStatus uint8

const (
	stPending  entryStatus = iota // someone is waiting for the resolution
	stResolved entryStatus = iota // resolved
)

const (
	CacheNoLocalReplies uint8 = 1 << iota // not found in local network (stored in the cache so we skip another local lookup or some time)
)

// shuffleAnswers reorders answers for very basic load balancing
func shuffleAnswers(answers []dns.RR) []dns.RR {
	n := len(answers)
	if n > 1 {
		rand.Seed(time.Now().UTC().UnixNano())

		for i := 0; i < n; i++ {
			r := i + rand.Intn(n-i)
			answers[r], answers[i] = answers[i], answers[r]
		}
	}

	return answers
}

// a cache entry
type cacheEntry struct {
	Status   entryStatus // status of the entry
	Flags    uint8       // some extra flags
	ReplyLen int

	question dns.Question
	protocol dnsProtocol
	reply    dns.Msg

	validUntil time.Time // obtained from the reply and stored here for convenience/speed
	putTime    time.Time

	index int // for fast lookups in the heap
}

func newCacheEntry(question *dns.Question, now time.Time) *cacheEntry {
	e := &cacheEntry{
		Status:     stPending,
		validUntil: now.Add(time.Second * time.Duration(defPendingTimeout)),
		question:   *question,
		index:      -1,
	}

	return e
}

// Get a copy of the reply stored in the entry, but with some values adjusted like the TTL
func (e *cacheEntry) getReply(request *dns.Msg, maxLen int, now time.Time) (*dns.Msg, error) {
	if e.Status != stResolved {
		return nil, nil
	}

	// if the reply has expired or is invalid, force the caller to start a new resolution
	if e.hasExpired(now) {
		return nil, nil
	}

	if e.Flags&CacheNoLocalReplies != 0 {
		return nil, errNoLocalReplies
	}

	if e.ReplyLen >= maxLen {
		Debug.Printf("[cache msgid %d] returning truncated reponse: %d > %d", request.MsgHdr.Id, e.ReplyLen, maxLen)
		return makeTruncatedReply(request), nil
	}

	// create a copy of the reply, with values for this particular query
	reply := e.reply.Copy()
	reply.SetReply(request)

	// adjust the TTLs
	passedSecs := uint32(now.Sub(e.putTime).Seconds())
	for _, rr := range reply.Answer {
		hdr := rr.Header()
		ttl := hdr.Ttl
		if passedSecs < ttl {
			hdr.Ttl = ttl - passedSecs
		} else {
			return nil, nil // it is expired: do not spend more time and return nil...
		}
	}

	reply.Rcode = e.reply.Rcode
	reply.Authoritative = true

	// shuffle the values, etc...
	reply.Answer = shuffleAnswers(reply.Answer)

	return reply, nil
}

func (e cacheEntry) hasExpired(now time.Time) bool {
	return e.validUntil.Before(now) || e.validUntil == now
}

// set the reply for the entry
// returns True if the entry has changed the validUntil time
func (e *cacheEntry) setReply(reply *dns.Msg, ttl int, flags uint8, now time.Time) bool {
	var prevValidUntil time.Time
	if e.Status == stResolved {
		if reply != nil {
			Debug.Printf("[cache msgid %d] replacing response in cache", reply.MsgHdr.Id)
		}
		prevValidUntil = e.validUntil
	}

	e.Status = stResolved
	e.Flags = flags
	e.putTime = now

	if ttl != nullTTL {
		e.validUntil = now.Add(time.Second * time.Duration(ttl))
	} else if reply != nil {
		// calculate the validUntil from the reply TTL
		var minTTL uint32 = math.MaxUint32
		for _, rr := range reply.Answer {
			ttl := rr.Header().Ttl
			if ttl < minTTL {
				minTTL = ttl // TODO: improve the minTTL calculation (maybe we should skip some RRs)
			}
		}
		e.validUntil = now.Add(time.Second * time.Duration(minTTL))
	}

	if reply != nil {
		e.reply = *reply
		e.ReplyLen = reply.Len()
	}

	return (prevValidUntil != e.validUntil)
}

//////////////////////////////////////////////////////////////////////////////////////

// An entriesPtrHeap is a min-heap of cache entries.
type entriesPtrsHeap []*cacheEntry

func (h entriesPtrsHeap) Len() int           { return len(h) }
func (h entriesPtrsHeap) Less(i, j int) bool { return h[i].validUntil.Before(h[j].validUntil) }
func (h entriesPtrsHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *entriesPtrsHeap) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	n := len(*h)
	entry := x.(*cacheEntry)
	entry.index = n
	*h = append(*h, entry)
}

func (h *entriesPtrsHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	item.index = -1 // for safety
	*h = old[0 : n-1]
	return item
}

//////////////////////////////////////////////////////////////////////////////////////

type cacheKey dns.Question
type entries map[cacheKey]*cacheEntry

// Cache is a thread-safe fixed capacity LRU cache.
type Cache struct {
	Capacity int

	entries  entries
	entriesH entriesPtrsHeap // len(entriesH) <= len(entries), as pending entries can be in entries but not in entriesH
	lock     sync.RWMutex
}

// NewCache creates a cache of the given capacity
func NewCache(capacity int) (*Cache, error) {
	if capacity <= 0 {
		return nil, errInvalidCapacity
	}
	c := &Cache{
		Capacity: capacity,
		entries:  make(entries, capacity),
	}

	heap.Init(&c.entriesH)
	return c, nil
}

// Clear removes all the entries in the cache
func (c *Cache) Clear() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.entries = make(entries, c.Capacity)
	heap.Init(&c.entriesH)
}

// Purge removes the old elements in the cache
func (c *Cache) Purge(now time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()

	for i, entry := range c.entriesH {
		if entry.hasExpired(now) {
			heap.Remove(&c.entriesH, i)
			delete(c.entries, cacheKey(entry.question))
		} else {
			return // all remaining entries must be still valid...
		}
	}
}

// Add adds a reply to the cache.
func (c *Cache) Put(request *dns.Msg, reply *dns.Msg, ttl int, flags uint8, now time.Time) int {
	c.lock.Lock()
	defer c.lock.Unlock()

	question := request.Question[0]
	key := cacheKey(question)
	ent, found := c.entries[key]
	if found {
		updated := ent.setReply(reply, ttl, flags, now)
		if updated {
			heap.Fix(&c.entriesH, ent.index)
		}
	} else {
		// If we will add a new item and the capacity has been exceeded, make some room...
		if len(c.entriesH) >= c.Capacity {
			lowestEntry := heap.Pop(&c.entriesH).(*cacheEntry)
			delete(c.entries, cacheKey(lowestEntry.question))
		}
		ent = newCacheEntry(&question, now)
		ent.setReply(reply, ttl, flags, now)
		heap.Push(&c.entriesH, ent)
		c.entries[key] = ent
	}
	return ent.ReplyLen
}

// Look up for a question's reply from the cache.
// If no reply is stored in the cache, it returns a `nil` reply and no error. The caller can then `Wait()`
// for another goroutine `Put`ing a reply in the cache.
func (c *Cache) Get(request *dns.Msg, maxLen int, now time.Time) (reply *dns.Msg, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	question := request.Question[0]
	key := cacheKey(question)
	if ent, found := c.entries[key]; found {
		reply, err = ent.getReply(request, maxLen, now)
		if ent.hasExpired(now) {
			Debug.Printf("[cache msgid %d] expired: removing", request.MsgHdr.Id)
			if ent.index > 0 {
				heap.Remove(&c.entriesH, ent.index)
			}
			delete(c.entries, key)
			reply = nil
		}
	} else {
		// we are the first asking for this name: create an entry with no reply... the caller must wait
		Debug.Printf("[cache msgid %d] addind in pending state", request.MsgHdr.Id)
		c.entries[key] = newCacheEntry(&question, now)
	}
	return
}

// Remove removes the provided question from the cache.
func (c *Cache) Remove(question *dns.Question) {
	c.lock.Lock()
	defer c.lock.Unlock()

	key := cacheKey(*question)
	if entry, found := c.entries[key]; found {
		if entry.index > 0 {
			heap.Remove(&c.entriesH, entry.index)
		}
		delete(c.entries, key)
	}
}

// Len returns the number of entries in the cache.
func (c *Cache) Len() int {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return len(c.entries)
}
