package sdk

import "container/list"

// lruSet is a fixed-capacity set that forgets its least recently added or
// touched keys. The event processor uses it to report each
// (flag, variant, context) exposure once rather than on every evaluation,
// without letting memory grow with the number of distinct contexts.
type lruSet struct {
	cap   int
	order *list.List // front is most recent
	items map[string]*list.Element
}

func newLRUSet(capacity int) *lruSet {
	if capacity < 1 {
		capacity = 1
	}
	return &lruSet{cap: capacity, order: list.New(), items: make(map[string]*list.Element, capacity)}
}

// Add inserts key and reports whether it was absent. A key that is already
// present is refreshed to most recent.
func (s *lruSet) Add(key string) bool {
	if el, ok := s.items[key]; ok {
		s.order.MoveToFront(el)
		return false
	}
	s.items[key] = s.order.PushFront(key)
	if s.order.Len() > s.cap {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.items, oldest.Value.(string))
	}
	return true
}

// Len returns the number of keys held.
func (s *lruSet) Len() int { return s.order.Len() }
