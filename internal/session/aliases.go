package session

import (
	"sync"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// remoteAliases is the receive-side ExprId → full-key mapping. Populated
// by inbound D_KEYEXPR, consumed by any dispatcher that sees a WireExpr
// with Scope != 0 (PUSH, REQUEST, DECLARE[D_TOKEN/U_TOKEN/…]).
//
// zenoh-rust's DeclareKeyExpr carries (ExprID, Scope, Suffix): ExprID is
// the alias being declared, Scope is an optional base alias to prefix,
// Suffix extends it. Resolution is the same concat formula applied to
// WireExpr at use time.
type remoteAliases struct {
	mu   sync.RWMutex
	byID map[uint16]string
}

func (s *Session) regRemoteAliases() *remoteAliases {
	s.remoteAliasesOnce.Do(func() {
		s.remoteAliases = &remoteAliases{byID: map[uint16]string{}}
	})
	return s.remoteAliases
}

// declareRemoteAlias records an alias binding. scope=0 means the alias
// is just suffix; scope != 0 means suffix extends the key that scope
// already denotes.
//
// We store the fully-concatenated key, not a scope reference. A later
// U_KEYEXPR on the base alias therefore does NOT invalidate aliases
// declared transitively on top of it — matching zenoh-rust, which also
// snapshots the key at declare time. (The router never retracts a
// parent alias while a child is still in use in practice.)
func (s *Session) declareRemoteAlias(exprID uint16, scope uint16, suffix string) {
	reg := s.regRemoteAliases()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if scope == 0 {
		reg.byID[exprID] = suffix
		return
	}
	base, ok := reg.byID[scope]
	if !ok {
		// Stale scope — treat as empty base, same as zenoh-rust's
		// best-effort behaviour. Future D_KEYEXPRs may still arrive.
		reg.byID[exprID] = suffix
		return
	}
	reg.byID[exprID] = base + suffix
}

func (s *Session) undeclareRemoteAlias(exprID uint16) {
	reg := s.regRemoteAliases()
	reg.mu.Lock()
	delete(reg.byID, exprID)
	reg.mu.Unlock()
}

// ResetRemoteAliases drops every inbound alias. The reconnect orchestrator
// calls this before re-registering entities on the new link because the
// router's fresh session has not issued any D_KEYEXPR yet and may reuse
// the same ExprIDs for different keys.
func (s *Session) ResetRemoteAliases() {
	reg := s.regRemoteAliases()
	reg.mu.Lock()
	reg.byID = map[uint16]string{}
	reg.mu.Unlock()
}

// resolveRemoteKey expands a WireExpr into its full string form using
// the inbound alias table. Returns (key, true) on success, ("", false)
// if the scope references an unknown alias — which a well-behaved router
// should never send after the handshake, but a reconnect mid-stream
// could conceivably cause.
func (s *Session) resolveRemoteKey(we wire.WireExpr) (string, bool) {
	if we.Scope == 0 {
		return we.Suffix, true
	}
	reg := s.regRemoteAliases()
	reg.mu.RLock()
	base, ok := reg.byID[we.Scope]
	reg.mu.RUnlock()
	if !ok {
		return "", false
	}
	return base + we.Suffix, true
}
