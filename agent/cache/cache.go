// Package cache provides caching features for data from a Consul server.
//
// While this is similar in some ways to the "agent/ae" package, a key
// difference is that with anti-entropy, the agent is the authoritative
// source so it resolves differences the server may have. With caching (this
// package), the server is the authoritative source and we do our best to
// balance performance and correctness, depending on the type of data being
// requested.
//
// The types of data that can be cached is configurable via the Type interface.
// This allows specialized behavior for certain types of data. Each type of
// Consul data (CA roots, leaf certs, intentions, KV, catalog, etc.) will
// have to be manually implemented. This usually is not much work, see
// the "agent/cache-types" package.
package cache

import (
	"container/heap"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/armon/go-metrics"
)

//go:generate mockery -all -inpkg

// Cache is a agent-local cache of Consul data. Create a Cache using the
// New function. A zero-value Cache is not ready for usage and will result
// in a panic.
//
// The types of data to be cached must be registered via RegisterType. Then,
// calls to Get specify the type and a Request implementation. The
// implementation of Request is usually done directly on the standard RPC
// struct in agent/structs.  This API makes cache usage a mostly drop-in
// replacement for non-cached RPC calls.
//
// The cache is partitioned by ACL and datacenter. This allows the cache
// to be safe for multi-DC queries and for queries where the data is modified
// due to ACLs all without the cache having to have any clever logic, at
// the slight expense of a less perfect cache.
//
// The Cache exposes various metrics via go-metrics. Please view the source
// searching for "metrics." to see the various metrics exposed. These can be
// used to explore the performance of the cache.
type Cache struct {
	// Keeps track of the cache hits and misses in total. This is used by
	// tests currently to verify cache behavior and is not meant for general
	// analytics; for that, go-metrics emitted values are better.
	hits, misses uint64

	// types stores the list of data types that the cache knows how to service.
	// These can be dynamically registered with RegisterType.
	typesLock sync.RWMutex
	types     map[string]typeEntry

	// entries contains the actual cache data. Access to entries and
	// entriesExpiryHeap must be protected by entriesLock.
	//
	// entriesExpiryHeap is a heap of *cacheEntry values ordered by
	// expiry, with the soonest to expire being first in the list (index 0).
	//
	// NOTE(mitchellh): The entry map key is currently a string in the format
	// of "<DC>/<ACL token>/<Request key>" in order to properly partition
	// requests to different datacenters and ACL tokens. This format has some
	// big drawbacks: we can't evict by datacenter, ACL token, etc. For an
	// initial implementaiton this works and the tests are agnostic to the
	// internal storage format so changing this should be possible safely.
	entriesLock       sync.RWMutex
	entries           map[string]cacheEntry
	entriesExpiryHeap *expiryHeap
}

// typeEntry is a single type that is registered with a Cache.
type typeEntry struct {
	Type Type
	Opts *RegisterOptions
}

// Options are options for the Cache.
type Options struct {
	// Nothing currently, reserved.
}

// New creates a new cache with the given RPC client and reasonable defaults.
// Further settings can be tweaked on the returned value.
func New(*Options) *Cache {
	// Initialize the heap. The buffer of 1 is really important because
	// its possible for the expiry loop to trigger the heap to update
	// itself and it'd block forever otherwise.
	h := &expiryHeap{NotifyCh: make(chan struct{}, 1)}
	heap.Init(h)

	c := &Cache{
		types:             make(map[string]typeEntry),
		entries:           make(map[string]cacheEntry),
		entriesExpiryHeap: h,
	}

	// Start the expiry watcher
	go c.runExpiryLoop()

	return c
}

// RegisterOptions are options that can be associated with a type being
// registered for the cache. This changes the behavior of the cache for
// this type.
type RegisterOptions struct {
	// LastGetTTL is the time that the values returned by this type remain
	// in the cache after the last get operation. If a value isn't accessed
	// within this duration, the value is purged from the cache and
	// background refreshing will cease.
	LastGetTTL time.Duration

	// Refresh configures whether the data is actively refreshed or if
	// the data is only refreshed on an explicit Get. The default (false)
	// is to only request data on explicit Get.
	Refresh bool

	// RefreshTimer is the time between attempting to refresh data.
	// If this is zero, then data is refreshed immediately when a fetch
	// is returned.
	//
	// RefreshTimeout determines the maximum query time for a refresh
	// operation. This is specified as part of the query options and is
	// expected to be implemented by the Type itself.
	//
	// Using these values, various "refresh" mechanisms can be implemented:
	//
	//   * With a high timer duration and a low timeout, a timer-based
	//     refresh can be set that minimizes load on the Consul servers.
	//
	//   * With a low timer and high timeout duration, a blocking-query-based
	//     refresh can be set so that changes in server data are recognized
	//     within the cache very quickly.
	//
	RefreshTimer   time.Duration
	RefreshTimeout time.Duration
}

// RegisterType registers a cacheable type.
//
// This makes the type available for Get but does not automatically perform
// any prefetching. In order to populate the cache, Get must be called.
func (c *Cache) RegisterType(n string, typ Type, opts *RegisterOptions) {
	if opts == nil {
		opts = &RegisterOptions{}
	}
	if opts.LastGetTTL == 0 {
		opts.LastGetTTL = 72 * time.Hour // reasonable default is days
	}

	c.typesLock.Lock()
	defer c.typesLock.Unlock()
	c.types[n] = typeEntry{Type: typ, Opts: opts}
}

// Get loads the data for the given type and request. If data satisfying the
// minimum index is present in the cache, it is returned immediately. Otherwise,
// this will block until the data is available or the request timeout is
// reached.
//
// Multiple Get calls for the same Request (matching CacheKey value) will
// block on a single network request.
//
// The timeout specified by the Request will be the timeout on the cache
// Get, and does not correspond to the timeout of any background data
// fetching. If the timeout is reached before data satisfying the minimum
// index is retrieved, the last known value (maybe nil) is returned. No
// error is returned on timeout. This matches the behavior of Consul blocking
// queries.
func (c *Cache) Get(t string, r Request) (interface{}, error) {
	info := r.CacheInfo()
	if info.Key == "" {
		metrics.IncrCounter([]string{"consul", "cache", "bypass"}, 1)

		// If no key is specified, then we do not cache this request.
		// Pass directly through to the backend.
		return c.fetchDirect(t, r)
	}

	// Get the actual key for our entry
	key := c.entryKey(&info)

	// First time through
	first := true

	// timeoutCh for watching our tmeout
	var timeoutCh <-chan time.Time

RETRY_GET:
	// Get the current value
	c.entriesLock.RLock()
	entry, ok := c.entries[key]
	c.entriesLock.RUnlock()

	// If we have a current value and the index is greater than the
	// currently stored index then we return that right away. If the
	// index is zero and we have something in the cache we accept whatever
	// we have.
	if ok && entry.Valid {
		if info.MinIndex == 0 || info.MinIndex < entry.Index {
			if first {
				metrics.IncrCounter([]string{"consul", "cache", t, "hit"}, 1)
				atomic.AddUint64(&c.hits, 1)
			}

			// Touch the expiration and fix the heap.
			c.entriesLock.Lock()
			entry.Expiry.Reset()
			c.entriesExpiryHeap.Fix(entry.Expiry)
			c.entriesLock.Unlock()

			return entry.Value, entry.Error
		}
	}

	// If this isn't our first time through and our last value has an error,
	// then we return the error. This has the behavior that we don't sit in
	// a retry loop getting the same error for the entire duration of the
	// timeout. Instead, we make one effort to fetch a new value, and if
	// there was an error, we return.
	if !first && entry.Error != nil {
		return entry.Value, entry.Error
	}

	if first {
		// Record the miss if its our first time through
		atomic.AddUint64(&c.misses, 1)

		// We increment two different counters for cache misses depending on
		// whether we're missing because we didn't have the data at all,
		// or if we're missing because we're blocking on a set index.
		if info.MinIndex == 0 {
			metrics.IncrCounter([]string{"consul", "cache", t, "miss_new"}, 1)
		} else {
			metrics.IncrCounter([]string{"consul", "cache", t, "miss_block"}, 1)
		}
	}

	// No longer our first time through
	first = false

	// Set our timeout channel if we must
	if info.Timeout > 0 && timeoutCh == nil {
		timeoutCh = time.After(info.Timeout)
	}

	// At this point, we know we either don't have a value at all or the
	// value we have is too old. We need to wait for new data.
	waiterCh, err := c.fetch(t, key, r, true)
	if err != nil {
		return nil, err
	}

	select {
	case <-waiterCh:
		// Our fetch returned, retry the get from the cache
		goto RETRY_GET

	case <-timeoutCh:
		// Timeout on the cache read, just return whatever we have.
		return entry.Value, nil
	}
}

// entryKey returns the key for the entry in the cache. See the note
// about the entry key format in the structure docs for Cache.
func (c *Cache) entryKey(r *RequestInfo) string {
	return fmt.Sprintf("%s/%s/%s", r.Datacenter, r.Token, r.Key)
}

// fetch triggers a new background fetch for the given Request. If a
// background fetch is already running for a matching Request, the waiter
// channel for that request is returned. The effect of this is that there
// is only ever one blocking query for any matching requests.
//
// If allowNew is true then the fetch should create the cache entry
// if it doesn't exist. If this is false, then fetch will do nothing
// if the entry doesn't exist. This latter case is to support refreshing.
func (c *Cache) fetch(t, key string, r Request, allowNew bool) (<-chan struct{}, error) {
	// Get the type that we're fetching
	c.typesLock.RLock()
	tEntry, ok := c.types[t]
	c.typesLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown type in cache: %s", t)
	}

	// We acquire a write lock because we may have to set Fetching to true.
	c.entriesLock.Lock()
	defer c.entriesLock.Unlock()
	entry, ok := c.entries[key]

	// If we aren't allowing new values and we don't have an existing value,
	// return immediately. We return an immediately-closed channel so nothing
	// blocks.
	if !ok && !allowNew {
		ch := make(chan struct{})
		close(ch)
		return ch, nil
	}

	// If we already have an entry and it is actively fetching, then return
	// the currently active waiter.
	if ok && entry.Fetching {
		return entry.Waiter, nil
	}

	// If we don't have an entry, then create it. The entry must be marked
	// as invalid so that it isn't returned as a valid value for a zero index.
	if !ok {
		entry = cacheEntry{Valid: false, Waiter: make(chan struct{})}
	}

	// Set that we're fetching to true, which makes it so that future
	// identical calls to fetch will return the same waiter rather than
	// perform multiple fetches.
	entry.Fetching = true
	c.entries[key] = entry
	metrics.SetGauge([]string{"consul", "cache", "entries_count"}, float32(len(c.entries)))

	// The actual Fetch must be performed in a goroutine.
	go func() {
		// Start building the new entry by blocking on the fetch.
		result, err := tEntry.Type.Fetch(FetchOptions{
			MinIndex: entry.Index,
			Timeout:  tEntry.Opts.RefreshTimeout,
		}, r)

		if err == nil {
			metrics.IncrCounter([]string{"consul", "cache", "fetch_success"}, 1)
			metrics.IncrCounter([]string{"consul", "cache", t, "fetch_success"}, 1)
		} else {
			metrics.IncrCounter([]string{"consul", "cache", "fetch_error"}, 1)
			metrics.IncrCounter([]string{"consul", "cache", t, "fetch_error"}, 1)
		}

		// Copy the existing entry to start.
		newEntry := entry
		newEntry.Fetching = false
		if result.Value != nil {
			// A new value was given, so we create a brand new entry.
			newEntry.Value = result.Value
			newEntry.Index = result.Index
			newEntry.Error = err

			// This is a valid entry with a result
			newEntry.Valid = true
		}

		// If we have an error and the prior entry wasn't valid, then we
		// set the error at least.
		if err != nil && !newEntry.Valid {
			newEntry.Error = err
		}

		// Create a new waiter that will be used for the next fetch.
		newEntry.Waiter = make(chan struct{})

		// Set our entry
		c.entriesLock.Lock()

		// If this is a new entry (not in the heap yet), then setup the
		// initial expiry information and insert. If we're already in
		// the heap we do nothing since we're reusing the same entry.
		if newEntry.Expiry == nil || newEntry.Expiry.HeapIndex == -1 {
			newEntry.Expiry = &cacheEntryExpiry{
				Key: key,
				TTL: tEntry.Opts.LastGetTTL,
			}
			newEntry.Expiry.Reset()
			heap.Push(c.entriesExpiryHeap, newEntry.Expiry)
		}

		c.entries[key] = newEntry
		c.entriesLock.Unlock()

		// Trigger the old waiter
		close(entry.Waiter)

		// If refresh is enabled, run the refresh in due time. The refresh
		// below might block, but saves us from spawning another goroutine.
		if tEntry.Opts.Refresh {
			c.refresh(tEntry.Opts, t, key, r)
		}
	}()

	return entry.Waiter, nil
}

// fetchDirect fetches the given request with no caching. Because this
// bypasses the caching entirely, multiple matching requests will result
// in multiple actual RPC calls (unlike fetch).
func (c *Cache) fetchDirect(t string, r Request) (interface{}, error) {
	// Get the type that we're fetching
	c.typesLock.RLock()
	tEntry, ok := c.types[t]
	c.typesLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown type in cache: %s", t)
	}

	// Fetch it with the min index specified directly by the request.
	result, err := tEntry.Type.Fetch(FetchOptions{
		MinIndex: r.CacheInfo().MinIndex,
	}, r)
	if err != nil {
		return nil, err
	}

	// Return the result and ignore the rest
	return result.Value, nil
}

// refresh triggers a fetch for a specific Request according to the
// registration options.
func (c *Cache) refresh(opts *RegisterOptions, t string, key string, r Request) {
	// Sanity-check, we should not schedule anything that has refresh disabled
	if !opts.Refresh {
		return
	}

	// If we have a timer, wait for it
	if opts.RefreshTimer > 0 {
		time.Sleep(opts.RefreshTimer)
	}

	// Trigger. The "allowNew" field is false because in the time we were
	// waiting to refresh we may have expired and got evicted. If that
	// happened, we don't want to create a new entry.
	c.fetch(t, key, r, false)
}

// runExpiryLoop is a blocking function that watches the expiration
// heap and invalidates entries that have expired.
func (c *Cache) runExpiryLoop() {
	var expiryTimer *time.Timer
	for {
		// If we have a previous timer, stop it.
		if expiryTimer != nil {
			expiryTimer.Stop()
		}

		// Get the entry expiring soonest
		var entry *cacheEntryExpiry
		var expiryCh <-chan time.Time
		c.entriesLock.RLock()
		if len(c.entriesExpiryHeap.Entries) > 0 {
			entry = c.entriesExpiryHeap.Entries[0]
			expiryTimer = time.NewTimer(entry.Expires.Sub(time.Now()))
			expiryCh = expiryTimer.C
		}
		c.entriesLock.RUnlock()

		select {
		case <-c.entriesExpiryHeap.NotifyCh:
			// Entries changed, so the heap may have changed. Restart loop.

		case <-expiryCh:
			c.entriesLock.Lock()

			// Entry expired! Remove it.
			delete(c.entries, entry.Key)
			heap.Remove(c.entriesExpiryHeap, entry.HeapIndex)

			// This is subtle but important: if we race and simultaneously
			// evict and fetch a new value, then we set this to -1 to
			// have it treated as a new value so that the TTL is extended.
			entry.HeapIndex = -1

			c.entriesLock.Unlock()

			metrics.IncrCounter([]string{"consul", "cache", "evict_expired"}, 1)
		}
	}
}

// Returns the number of cache hits. Safe to call concurrently.
func (c *Cache) Hits() uint64 {
	return atomic.LoadUint64(&c.hits)
}
