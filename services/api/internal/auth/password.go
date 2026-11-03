package auth

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// Password login (ADR-035, contracts §15): bcrypt hashes in users.password_hash.

// PasswordCost is the bcrypt work factor of new hashes (~250 ms on a laptop).
const PasswordCost = 12

// Password length bounds: bcrypt only reads the first 72 bytes, so longer ones are refused rather than truncated.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 72
)

// ErrBadPassword is returned when a password does not match its hash (or there is no hash).
var ErrBadPassword = errors.New("invalid email or password")

// ValidatePassword enforces the length bounds (characters for the minimum, bytes for the bcrypt maximum).
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	return nil
}

// HashPassword validates and hashes pw.
func HashPassword(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), PasswordCost)
	return string(h), err
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// CheckPassword compares pw with hash in constant time. An empty hash (unknown user, or a user without a password)
// still runs a full bcrypt comparison against a dummy hash, so the response time does not reveal which accounts exist.
func CheckPassword(hash, pw string) error {
	if hash == "" {
		dummyOnce.Do(func() {
			dummyHash, _ = bcrypt.GenerateFromPassword([]byte("tt-dummy-password-for-timing"), PasswordCost)
		})
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw))
		return ErrBadPassword
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)); err != nil {
		return ErrBadPassword
	}
	return nil
}

// ---------------------------------------------------------------- failed attempt limiter

// Limiter counts failed logins per key (client IP, email) in a fixed window. It is in-memory (one api replica on the
// demo; several replicas each keep their own counts, so the effective limit is replicas × Max).
type Limiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time

	mu sync.Mutex
	m  map[string]window
}

type window struct {
	n     int
	start time.Time
}

// NewLimiter allows max failures per key per window.
func NewLimiter(max int, w time.Duration) *Limiter {
	return &Limiter{Max: max, Window: w, Now: time.Now, m: map[string]window{}}
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Blocked reports whether key has used up its failures; retry is how long until the window resets.
func (l *Limiter) Blocked(key string) (blocked bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.m[key]
	now := l.now()
	if !ok || now.Sub(w.start) >= l.Window {
		return false, 0
	}
	if w.n >= l.Max {
		return true, l.Window - now.Sub(w.start)
	}
	return false, 0
}

// Fail records a failed attempt for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.m == nil {
		l.m = map[string]window{}
	}
	if len(l.m) > 100000 { // bounded memory: drop expired windows, else everything
		for k, w := range l.m {
			if now.Sub(w.start) >= l.Window {
				delete(l.m, k)
			}
		}
		if len(l.m) > 100000 {
			l.m = map[string]window{}
		}
	}
	w, ok := l.m[key]
	if !ok || now.Sub(w.start) >= l.Window {
		w = window{start: now}
	}
	w.n++
	l.m[key] = w
}

// Reset forgets key (successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

// ClientIP is the caller's address: the first value of header when set (a header the reverse proxy in front of the
// api overwrites, e.g. CF-Connecting-IP behind Cloudflare Tunnel), else the TCP peer.
func ClientIP(r *http.Request, header string) string {
	if header != "" {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(header), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
