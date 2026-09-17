package authz

import (
	"container/list"
	"sync"

	"github.com/cedar-policy/cedar-go"
)

const defaultEntityCacheSize = 1000

type entityCacheEntry struct {
	key      string
	entities cedar.EntityMap
}

// EntityCache is a bounded, thread-safe LRU cache of per-user Cedar entity
// graphs.
type EntityCache struct {
	mu      sync.Mutex
	maxSize int
	ll      *list.List
	entries map[string]*list.Element
}

func NewEntityCache(maxSize int) *EntityCache {
	if maxSize <= 0 {
		maxSize = defaultEntityCacheSize
	}
	return &EntityCache{
		maxSize: maxSize,
		ll:      list.New(),
		entries: make(map[string]*list.Element),
	}
}

func (c *EntityCache) Get(key string) (cedar.EntityMap, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(element)
	return element.Value.(entityCacheEntry).entities.Clone(), true
}

func (c *EntityCache) Put(key string, entities cedar.EntityMap) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value = entityCacheEntry{key: key, entities: entities.Clone()}
		c.ll.MoveToFront(element)
		return
	}
	element := c.ll.PushFront(entityCacheEntry{key: key, entities: entities.Clone()})
	c.entries[key] = element
	if c.ll.Len() > c.maxSize {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.entries, oldest.Value.(entityCacheEntry).key)
		}
	}
}

func (c *EntityCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.ll.Remove(element)
		delete(c.entries, key)
	}
}

func (c *EntityCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.entries = make(map[string]*list.Element)
}

func (c *EntityCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
