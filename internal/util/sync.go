package util

import (
	"sync"
	"time"
)

// WithMutex extends the Mutex type with the convenient .With(func) function
type WithMutex struct {
	sync.Mutex
}

// With executes the given function with the mutex locked
func (m *WithMutex) With(f func()) {
	m.Lock()
	defer m.Unlock()
	f()
}

// StringSetWithExpiry is a set of strings whose entries disappear after a given time.
type StringSetWithExpiry struct {
	data map[string]time.Time
	age  time.Duration
	lock WithMutex
}

// NewStringSetWithExpiry returns a new StringSetWithExpiry, with the given expiry duration.
func NewStringSetWithExpiry(d time.Duration) *StringSetWithExpiry {
	return &StringSetWithExpiry{data: make(map[string]time.Time), age: d}
}

// Add adds the given string to the set
func (ss *StringSetWithExpiry) Add(s string) {
	ss.lock.With(func() {
		ss.data[s] = time.Now()
	})
	ss.CheckExpire()
}

// CheckExpire walks the set and removes the entries which have expired.
func (ss *StringSetWithExpiry) CheckExpire() int {
	count := 0
	ss.lock.With(func() {
		for s, t := range ss.data {
			if time.Since(t) >= ss.age {
				delete(ss.data, s)
				count++
			}
		}
	})
	return count
}

// Has tests if a string is present and not expired in this set.
func (ss *StringSetWithExpiry) Has(s string) bool {
	var ok bool
	ss.lock.With(func() {
		var t time.Time
		t, ok = ss.data[s]
		// It may be there but be expired.
		ok = ok && time.Since(t) < ss.age
	})
	return ok
}

// TestAndSet atomically tests if the string s is present in the set and adds it if it isn't.
// Returns true iff it was in the set.
func (ss *StringSetWithExpiry) TestAndSet(s string) bool {
	var ok bool
	ss.lock.With(func() {
		var t time.Time
		t, ok = ss.data[s]
		if ok && time.Since(t) >= ss.age {
			// It's there but it's expired, so treat it as absent.
			ok = false
		}
		if !ok {
			ss.data[s] = time.Now()
		}
	})
	return ok
}
