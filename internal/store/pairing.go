package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNoSlot is returned for a pairing slot that does not exist, has
	// expired or belongs to another chain.
	ErrNoSlot = errors.New("no such pairing slot")
	// ErrSlotTaken is returned by CreatePairSlot when the slot ID is in use.
	ErrSlotTaken = errors.New("the pairing slot is taken")
	// ErrSlotUsed is returned when a step of a slot was done already, or the
	// step before it was not.
	ErrSlotUsed = errors.New("the pairing slot is used")
)

// PairSlot relays the messages of one device pairing: the existing device
// (A) and the new one (B) run SPAKE2 on a short code through it, and A sends
// B the chain's secret encrypted with the key they agreed on. The server sees
// only the PAKE messages and ciphertext; see docs/protocol.md.
type PairSlot struct {
	// ID is hex(SHA-256 of the slot part of the code, with a context).
	ID      string
	ChainID string
	// AMsg is the SPAKE2 message of A. BMsg, BName and BConfirm come when B
	// joins, Box when A approves.
	AMsg     []byte
	BMsg     []byte
	BName    string
	BConfirm []byte
	Box      []byte
	Created  time.Time
}

// PairStore keeps pairing slots. Slots older than ttl count as gone.
type PairStore interface {
	// CreatePairSlot stores a new slot, removing expired ones. It returns
	// ErrSlotTaken if a live slot has the ID, ErrNoChain if the chain does
	// not exist.
	CreatePairSlot(ctx context.Context, s PairSlot, ttl time.Duration) error
	PairSlot(ctx context.Context, id string, ttl time.Duration) (PairSlot, bool, error)
	// JoinPairSlot records the message of B, once.
	JoinPairSlot(ctx context.Context, id string, msg []byte, name string, confirm []byte, ttl time.Duration) error
	// ApprovePairSlot records the box of A, once, after B joined; only the
	// chain of the slot may.
	ApprovePairSlot(ctx context.Context, id, chainID string, box []byte, ttl time.Duration) error
	// DeletePairSlot removes the slot if it belongs to chainID.
	DeletePairSlot(ctx context.Context, id, chainID string) error
}
