// Package zenoh provides a pure-Go client for the Eclipse Zenoh protocol.
//
// It mirrors the public API of eclipse-zenoh/zenoh-go (which is a cgo binding
// to zenoh-c) but is implemented natively in Go. It speaks the Zenoh 1.0
// wire protocol (version 0x09) directly to a zenoh router or peer.
//
// # Scope
//
// Initial implementation supports Client mode over TCP unicast.
//
// # Thread safety
//
// All exported methods on Session, Publisher, Subscriber, Queryable, Querier,
// Query, and KeyExpr are safe for concurrent use unless otherwise documented.
package zenoh
