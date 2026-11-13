// Package rpc exposes the stable public API of Mini-RPC.
//
// Generated clients consume ClientConn. Services consume ServiceDesc and
// ServiceRegistrar. Context helpers mark calls as idempotent so client
// governance may retry only when application semantics permit it.
package rpc
