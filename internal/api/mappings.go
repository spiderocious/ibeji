package api

import (
	"github.com/feranmi/ibeji/internal/discovery"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// Every domain error the daemon's packages can surface gets exactly one stable
// API code here.
//
// Kept in one list so the mapping is reviewable as a whole: the failure that
// most damages an agent's behaviour is the same condition reporting under two
// different codes from two endpoints, and that is easy to introduce one handler
// at a time and hard to see afterwards.
//
// Messages are the human sentence for the code. They name the machine-level
// fact; the hint, added at the call site, names the likely cause.
func init() {
	RegisterMapping(identity.ErrKeyMismatch, CodeKeyMismatch,
		"the paired device presented a different key than the one pinned")
	RegisterMapping(identity.ErrNotPaired, CodeNotPaired,
		"no device is paired")
	RegisterMapping(identity.ErrAlreadyPaired, CodeAlreadyPaired,
		"a device is already paired (Ìbejì v1 pairs exactly two devices)")
	RegisterMapping(identity.ErrKeychainLocked, CodeInternal,
		"the keychain refused access")
	RegisterMapping(identity.ErrNotFound, CodeNotFound,
		"no such keychain item")

	RegisterMapping(link.ErrNotLinked, CodePeerOffline,
		"the paired device is not reachable")
	RegisterMapping(link.ErrClosed, CodePeerOffline,
		"the link to the paired device closed")
	RegisterMapping(link.ErrStreamRejected, CodeInternal,
		"the paired device rejected the request")
	RegisterMapping(link.ErrPairingExpired, CodePairingExpired,
		"this pairing attempt has expired")
	RegisterMapping(link.ErrPairingUnknown, CodeNotFound,
		"no such pairing attempt")
	RegisterMapping(link.ErrCodeMismatch, CodeCodeMismatch,
		"the pairing codes do not match")

	RegisterMapping(discovery.ErrNoAddress, CodeNotFound,
		"that device advertised no usable address")
	RegisterMapping(discovery.ErrMalformedRecord, CodeNotFound,
		"that device's advertisement is missing required fields")
}
