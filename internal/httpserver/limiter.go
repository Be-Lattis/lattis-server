package httpserver

import (
	"sync"
	"time"
)

// limiter : seau à jetons par adresse, pour /oauth/*, /login/* et /setup.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // jetons par seconde
	burst   float64
	buckets map[string]*bucket
	last    time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(perWindow int, window time.Duration) *limiter {
	return &limiter{rate: float64(perWindow) / window.Seconds(), burst: float64(perWindow), buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.at) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
