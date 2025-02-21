package cache

import (
	"context"
	"sync"
	"time"

	"github.com/gekatateam/go-generics-cache/policy/clock"
	"github.com/gekatateam/go-generics-cache/policy/fifo"
	"github.com/gekatateam/go-generics-cache/policy/lfu"
	"github.com/gekatateam/go-generics-cache/policy/lru"
	"github.com/gekatateam/go-generics-cache/policy/mru"
	"github.com/gekatateam/go-generics-cache/policy/simple"
)

// Interface is a common-cache interface.
type Interface[K comparable, V any] interface {
	// Get looks up a key's value from the cache.
	Get(key K) (value V, ok bool)
	// Set sets a value to the cache with key. replacing any existing value.
	Set(key K, val V)
	// Keys returns the keys of the cache. The order is relied on algorithms.
	Keys() []K
	// Delete deletes the item with provided key from the cache.
	Delete(key K)
}

var (
	_ = []Interface[struct{}, any]{
		(*simple.Cache[struct{}, any])(nil),
		(*lru.Cache[struct{}, any])(nil),
		(*lfu.Cache[struct{}, any])(nil),
		(*fifo.Cache[struct{}, any])(nil),
		(*mru.Cache[struct{}, any])(nil),
		(*clock.Cache[struct{}, any])(nil),
	}
)

// Item is an item
type Item[K comparable, V any] struct {
	Key        K
	Value      V
	Expiration time.Time
}

// Expired returns true if the item has expired.
func (item *Item[K, V]) Expired() bool {
	if item.Expiration.IsZero() {
		return false
	}
	return nowFunc().After(item.Expiration)
}

var nowFunc = time.Now

// ItemOption is an option for cache item.
type ItemOption func(*itemOptions)

type itemOptions struct {
	expiration time.Time // default none
}

// WithExpiration is an option to set expiration time for any items.
// If the expiration is zero or negative value, it treats as w/o expiration.
func WithExpiration(exp time.Duration) ItemOption {
	return func(o *itemOptions) {
		o.expiration = nowFunc().Add(exp)
	}
}

// newItem creates a new item with specified any options.
func newItem[K comparable, V any](key K, val V, opts ...ItemOption) *Item[K, V] {
	o := new(itemOptions)
	for _, optFunc := range opts {
		optFunc(o)
	}
	return &Item[K, V]{
		Key:        key,
		Value:      val,
		Expiration: o.expiration,
	}
}

// Cache is a thread safe cache.
type Cache[K comparable, V any] struct {
	cache Interface[K, *Item[K, V]]
	//expirations map[K]chan struct{}
	// mu is used to do lock in some method process.
	mu      sync.RWMutex
	janitor *janitor
}

// Option is an option for cache.
type Option[K comparable, V any] func(*options[K, V])

type options[K comparable, V any] struct {
	cache           Interface[K, *Item[K, V]]
	janitorInterval time.Duration
}

func newOptions[K comparable, V any]() *options[K, V] {
	return &options[K, V]{
		cache:           simple.NewCache[K, *Item[K, V]](),
		janitorInterval: time.Minute,
	}
}

// AsLRU is an option to make a new Cache as LRU algorithm.
func AsLRU[K comparable, V any](opts ...lru.Option) Option[K, V] {
	return func(o *options[K, V]) {
		o.cache = lru.NewCache[K, *Item[K, V]](opts...)
	}
}

// AsLFU is an option to make a new Cache as LFU algorithm.
func AsLFU[K comparable, V any](opts ...lfu.Option) Option[K, V] {
	return func(o *options[K, V]) {
		o.cache = lfu.NewCache[K, *Item[K, V]](opts...)
	}
}

// AsFIFO is an option to make a new Cache as FIFO algorithm.
func AsFIFO[K comparable, V any](opts ...fifo.Option) Option[K, V] {
	return func(o *options[K, V]) {
		o.cache = fifo.NewCache[K, *Item[K, V]](opts...)
	}
}

// AsMRU is an option to make a new Cache as MRU algorithm.
func AsMRU[K comparable, V any](opts ...mru.Option) Option[K, V] {
	return func(o *options[K, V]) {
		o.cache = mru.NewCache[K, *Item[K, V]](opts...)
	}
}

// AsClock is an option to make a new Cache as clock algorithm.
func AsClock[K comparable, V any](opts ...clock.Option) Option[K, V] {
	return func(o *options[K, V]) {
		o.cache = clock.NewCache[K, *Item[K, V]](opts...)
	}
}

// WithJanitorInterval is an option to specify how often cache should delete expired items.
//
// Default is 1 minute.
func WithJanitorInterval[K comparable, V any](d time.Duration) Option[K, V] {
	return func(o *options[K, V]) {
		o.janitorInterval = d
	}
}

// New creates a new thread safe Cache.
// The janitor will not be stopped which is created by this function. If you
// want to stop the janitor gracefully, You should use the `NewContext` function
// instead of this.
//
// There are several Cache replacement policies available with you specified any options.
func New[K comparable, V any](opts ...Option[K, V]) *Cache[K, V] {
	return NewContext(context.Background(), opts...)
}

// NewContext creates a new thread safe Cache with context.
// This function will be stopped an internal janitor when the context is cancelled.
//
// There are several Cache replacement policies available with you specified any options.
func NewContext[K comparable, V any](ctx context.Context, opts ...Option[K, V]) *Cache[K, V] {
	o := newOptions[K, V]()
	for _, optFunc := range opts {
		optFunc(o)
	}
	cache := &Cache[K, V]{
		cache:   o.cache,
		janitor: newJanitor(ctx, o.janitorInterval),
	}
	cache.janitor.run(cache.DeleteExpired)
	return cache
}

// Get looks up a key's value from the cache.
func (c *Cache[K, V]) Get(key K) (value V, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.cache.Get(key)

	if !ok {
		return
	}

	// Returns nil if the item has been expired.
	// Do not delete here and leave it to an external process such as Janitor.
	if item.Expired() {
		return value, false
	}

	return item.Value, true
}

// DeleteExpired all expired items from the cache.
func (c *Cache[K, V]) DeleteExpired() {
	c.mu.Lock()
	keys := c.cache.Keys()
	c.mu.Unlock()

	for _, key := range keys {
		c.mu.Lock()
		// if is expired, delete it and return nil instead
		item, ok := c.cache.Get(key)
		if ok && item.Expired() {
			c.cache.Delete(key)
		}
		c.mu.Unlock()
	}
}

// Set sets a value to the cache with key. replacing any existing value.
func (c *Cache[K, V]) Set(key K, val V, opts ...ItemOption) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := newItem(key, val, opts...)
	c.cache.Set(key, item)
}

// Keys returns the keys of the cache. the order is relied on algorithms.
func (c *Cache[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache.Keys()
}

func (c *Cache[K, V]) List() map[K]V {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := c.cache.Keys()
	items := make(map[K]V, len(keys))
	for _, v := range keys {
		item, ok := c.cache.Get(v)
		if ok {
			items[v] = item.Value
		}
	}

	return items
}

func (c *Cache[K, V]) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys := c.cache.Keys()
	for _, v := range keys {
		c.cache.Delete(v)
	}
}

// Delete deletes the item with provided key from the cache.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.Delete(key)
}

// Contains reports whether key is within cache.
func (c *Cache[K, V]) Contains(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.cache.Get(key)
	return ok
}

// NumberCache is a in-memory cache which is able to store only Number constraint.
type NumberCache[K comparable, V Number] struct {
	*Cache[K, V]
	// nmu is used to do lock in Increment/Decrement process.
	// Note that this must be here as a separate mutex because mu in Cache struct is Locked in Get,
	// and if we call mu.Lock in Increment/Decrement, it will cause deadlock.
	nmu sync.Mutex
}

// NewNumber creates a new cache for Number constraint.
func NewNumber[K comparable, V Number](opts ...Option[K, V]) *NumberCache[K, V] {
	return &NumberCache[K, V]{
		Cache: New(opts...),
	}
}

// Increment an item of type Number constraint by n.
// Returns the incremented value.
func (nc *NumberCache[K, V]) Increment(key K, n V) V {
	// In order to avoid lost update, we must lock whole Increment/Decrement process.
	nc.nmu.Lock()
	defer nc.nmu.Unlock()
	got, _ := nc.Cache.Get(key)
	nv := got + n
	nc.Cache.Set(key, nv)
	return nv
}

// Decrement an item of type Number constraint by n.
// Returns the decremented value.
func (nc *NumberCache[K, V]) Decrement(key K, n V) V {
	nc.nmu.Lock()
	defer nc.nmu.Unlock()
	got, _ := nc.Cache.Get(key)
	nv := got - n
	nc.Cache.Set(key, nv)
	return nv
}
