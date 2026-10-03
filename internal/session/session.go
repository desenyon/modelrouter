// Package session tracks conversation affinity: which model served the last
// turn of a conversation and how much of its prompt is likely warm in the
// provider's prompt cache. The optimizer prices cached input accordingly,
// which makes stickiness an emergent cost decision rather than a rule.
package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/lru"
)

// State is what the router remembers about a conversation.
type State struct {
	ModelID     string
	Provider    string
	TierRank    int
	InputTokens int
	At          time.Time
	Turns       int
}

// Store keeps conversation state.
type Store struct {
	c *lru.Cache[string, State]
}

// New builds a store retaining up to n conversations for ttl.
func New(n int, ttl time.Duration) *Store {
	return &Store{c: lru.New[string, State](n, 0, ttl)}
}

// Get returns the state for key.
func (s *Store) Get(key string) (State, bool) {
	if key == "" {
		return State{}, false
	}
	return s.c.Get(key)
}

// Put records the model that served a turn.
func (s *Store) Put(key string, st State) {
	if key != "" {
		s.c.Put(key, st, 1)
	}
}

// Key derives a stable conversation key: the client's session id if given,
// otherwise a hash of the conversation's stable prefix (system prompt, tool
// names, first user message).
func Key(req *canon.Request) string {
	if req.Router.SessionID != "" {
		return "s:" + req.Router.SessionID
	}
	h := sha256.New()
	h.Write([]byte(req.SystemText()))
	h.Write([]byte{0})
	for _, t := range req.Tools {
		h.Write([]byte(t.Name))
		h.Write([]byte{1})
	}
	firstUser := ""
	users := 0
	for _, m := range req.Messages {
		if m.Role == canon.RoleUser {
			users++
			if firstUser == "" {
				firstUser = m.Text()
			}
		}
	}
	if users < 2 && !req.InToolLoop() {
		return "" // single-turn requests have no conversation to stick to
	}
	h.Write([]byte(firstUser))
	return "h:" + hex.EncodeToString(h.Sum(nil)[:12])
}

// Fingerprints returns (full, previous): full hashes the whole message list;
// previous hashes the messages before the latest assistant turn, i.e. the
// "full" fingerprint of the request that produced that assistant turn.
// A repeated full fingerprint signals a regeneration; a matching previous
// fingerprint signals the user continued the conversation.
func Fingerprints(req *canon.Request) (full, prev string) {
	lastAsst := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == canon.RoleAssistant {
			lastAsst = i
			break
		}
	}
	h := sha256.New()
	var prevSum []byte
	for i, m := range req.Messages {
		if i == lastAsst {
			prevSum = h.Sum(nil)
		}
		writeMsg(h, m)
	}
	full = hex.EncodeToString(h.Sum(nil)[:16])
	if prevSum != nil {
		prev = hex.EncodeToString(prevSum[:16])
	}
	return
}

func writeMsg(h hash.Hash, m canon.Message) {
	var n [8]byte
	h.Write([]byte(m.Role))
	for _, p := range m.Parts {
		h.Write([]byte(p.Type))
		binary.LittleEndian.PutUint64(n[:], uint64(len(p.Text)))
		h.Write(n[:])
		h.Write([]byte(p.Text))
		h.Write([]byte(p.URL))
		if len(p.Data) > 64 {
			h.Write([]byte(p.Data[:64]))
		} else {
			h.Write([]byte(p.Data))
		}
	}
	for _, tc := range m.ToolCalls {
		h.Write([]byte(tc.Name))
		h.Write([]byte(tc.Arguments))
	}
	h.Write([]byte(m.ToolCallID))
	h.Write([]byte{0xff})
}
