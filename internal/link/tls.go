package link

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/feranmi/ibeji/internal/identity"
)

// The TLS layer binds a connection to a pinned Ed25519 identity.
//
// Ìbejì has no certificate authority and wants none: there is no third party on
// a LAN to vouch for anybody, and introducing one would mean either shipping a
// CA or trusting the internet, both of which the product rules out. Instead
// each device presents a self-signed certificate carrying its own Ed25519 key,
// and both sides verify that the key in the certificate is exactly the key they
// pinned at pairing.
//
// So the certificate is not an assertion of identity — it is just a carrier for
// the public key. All the trust lives in the pin, which is why normal
// certificate verification is disabled and replaced with a key comparison that
// cannot be satisfied by any amount of valid-looking PKI.

// certLifetime is how long a generated certificate claims to be valid.
//
// Nothing checks it in the normal case: validity windows exist to limit the
// damage of a leaked key in a PKI, and here the key is the identity. A long
// window avoids a device becoming unreachable because its clock drifted or
// because it stayed up for a year.
const certLifetime = 10 * 365 * 24 * time.Hour

// ErrNoPeerCertificate is returned when a peer completes a handshake without
// presenting a certificate. Both sides must present one: the link is mutually
// authenticated or it does not exist.
var ErrNoPeerCertificate = errors.New("link: peer presented no certificate")

// selfSignedCert builds this device's TLS certificate from its identity.
func selfSignedCert(id *identity.Identity, name string) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("link: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour), // tolerate a little clock skew
		NotAfter:     time.Now().Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.Public(), id.Private())
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("link: create certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  id.Private(),
		Leaf:        nil,
	}, nil
}

// peerKeyFrom extracts the Ed25519 public key a peer presented.
func peerKeyFrom(state tls.ConnectionState) (ed25519.PublicKey, error) {
	if len(state.PeerCertificates) == 0 {
		return nil, ErrNoPeerCertificate
	}
	pub, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		// A peer using a different key type is not an Ìbejì device, or is an
		// older one. Either way there is nothing to compare against a pin.
		return nil, fmt.Errorf("link: peer's certificate holds a %T, not an Ed25519 key",
			state.PeerCertificates[0].PublicKey)
	}
	return pub, nil
}

// KeyProvider reports the key to expect right now.
//
// The pin is read at handshake time rather than captured when the listener was
// built, because the pin changes while the listener is alive: unpairing must
// take effect immediately, and a listener holding a snapshot would go on
// accepting the device the user just revoked. That was a real bug — alpha
// accepted a connection from beta 35ms after unpairing it.
type KeyProvider func() ed25519.PublicKey

// verifyPinnedLive builds a verification callback that consults the provider
// on every handshake.
func verifyPinnedLive(current KeyProvider) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		got, err := peerKeyFrom(state)
		if err != nil {
			return err
		}
		expect := current()
		if expect == nil {
			// Unpaired: there is nobody this device is willing to talk to.
			return fmt.Errorf("%w: this device is not paired", identity.ErrNotPaired)
		}
		if !got.Equal(expect) {
			return fmt.Errorf("%w (presented %s, pinned %s)",
				identity.ErrKeyMismatch,
				identity.Fingerprint(got),
				identity.Fingerprint(expect))
		}
		return nil
	}
}

// verifyPinned builds the verification callback both sides use.
//
// It runs in place of certificate-chain validation, not alongside it: there is
// no chain to validate. The only question that matters is whether this is the
// key we pinned, and a mismatch must be reported as identity.ErrKeyMismatch so
// the daemon can raise a re-pair prompt rather than a generic TLS error.
func verifyPinned(expect ed25519.PublicKey) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		got, err := peerKeyFrom(state)
		if err != nil {
			return err
		}
		if !got.Equal(expect) {
			return fmt.Errorf("%w (presented %s, pinned %s)",
				identity.ErrKeyMismatch,
				identity.Fingerprint(got),
				identity.Fingerprint(expect))
		}
		return nil
	}
}

// tlsConfig builds the shared part of both sides' configuration.
func tlsConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		// There is no CA and no hostname to check. Verification is replaced
		// wholesale by the pinned-key comparison in VerifyConnection, which the
		// callers below install.
		InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyConnection
		NextProtos:         []string{"ibeji/1"},
	}
}

// ClientConfig builds the dialing side's TLS configuration.
//
// expect is the peer's pinned key. During pairing no key is pinned yet, so
// expect is nil and verification is deferred to the caller, which reads the
// presented key out of the handshake and shows its fingerprint beside the
// six-digit code. That is the one moment this product trusts a key it has not
// seen before, and it is gated on a human comparing numbers.
func ClientConfig(id *identity.Identity, name string, expect ed25519.PublicKey) (*tls.Config, error) {
	cert, err := selfSignedCert(id, name)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig(cert)
	if expect != nil {
		cfg.VerifyConnection = verifyPinned(expect)
	} else {
		cfg.VerifyConnection = requirePeerCertificate
	}
	return cfg, nil
}

// ServerConfig builds the accepting side's TLS configuration.
//
// The peer certificate is always required: an unauthenticated inbound
// connection is never useful to this product, so it is refused at the
// handshake rather than later.
func ServerConfig(id *identity.Identity, name string, expect ed25519.PublicKey) (*tls.Config, error) {
	cert, err := selfSignedCert(id, name)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig(cert)
	cfg.ClientAuth = tls.RequireAnyClientCert
	if expect != nil {
		cfg.VerifyConnection = verifyPinned(expect)
	} else {
		cfg.VerifyConnection = requirePeerCertificate
	}
	return cfg, nil
}

// ServerConfigLive builds an accepting configuration that reads the pin at
// handshake time.
//
// Used by the daemon's long-lived listener, where the pinned key changes
// underneath it: unpair, then re-pair with a different machine, all without
// restarting. A nil provider, or one returning nil, means unpaired — and an
// unpaired device accepts nobody, except during the pairing ceremony which
// uses ServerConfigPairing.
func ServerConfigLive(id *identity.Identity, name string, current KeyProvider) (*tls.Config, error) {
	cert, err := selfSignedCert(id, name)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig(cert)
	cfg.ClientAuth = tls.RequireAnyClientCert
	cfg.VerifyConnection = verifyPinnedLive(current)
	return cfg, nil
}

// ServerConfigEither accepts the pinned peer when one is pinned, and any
// certificate-presenting peer when none is.
//
// This is what the daemon's listener uses. An unpaired device accepts a
// stranger — that is how pairing starts, and the other machine may be the one
// that clicks first — but grants nothing by it: the connection serves control
// streams only until a key is pinned. Once one is, that key alone is accepted.
func ServerConfigEither(id *identity.Identity, name string, current KeyProvider) (*tls.Config, error) {
	cert, err := selfSignedCert(id, name)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig(cert)
	cfg.ClientAuth = tls.RequireAnyClientCert
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		got, err := peerKeyFrom(state)
		if err != nil {
			return err
		}
		expect := current()
		if expect == nil {
			// Unpaired: accept, because this is how pairing starts. Whoever
			// is at the other machine clicks first, and this side has no
			// ceremony of its own yet — gating on one meant a device could
			// only ever be paired *from* here, never *to* here.
			//
			// Accepting here is safe because an unpaired device grants nothing
			// by connecting: servePairing allows control streams only, and
			// trust still rests entirely on a human comparing six digits.
			//
			// What stops an ex-peer walking back in after unpairing is not
			// this check but servePairing refusing to treat the connection as
			// a link, so neither side reports itself linked.
			return nil
		}
		if !got.Equal(expect) {
			return fmt.Errorf("%w (presented %s, pinned %s)",
				identity.ErrKeyMismatch,
				identity.Fingerprint(got),
				identity.Fingerprint(expect))
		}
		return nil
	}
	return cfg, nil
}

// ServerConfigPairing accepts any key but requires one, for the ceremony.
func ServerConfigPairing(id *identity.Identity, name string) (*tls.Config, error) {
	return ServerConfig(id, name, nil)
}

// requirePeerCertificate is the pairing-time check: we do not yet know which
// key to expect, but the peer must present one so the ceremony has something
// to show a fingerprint for.
func requirePeerCertificate(state tls.ConnectionState) error {
	_, err := peerKeyFrom(state)
	return err
}

// PeerKey reads the authenticated peer's public key from a completed handshake.
//
// Safe to trust only after the handshake succeeded: by then VerifyConnection
// has either matched it against the pin or — during pairing — confirmed that a
// key was presented at all.
func PeerKey(conn *tls.Conn) (ed25519.PublicKey, error) {
	state := conn.ConnectionState()
	if !state.HandshakeComplete {
		return nil, errors.New("link: handshake has not completed")
	}
	return peerKeyFrom(state)
}

// parseLeaf parses a certificate's leaf, so callers can read the subject and
// key without re-deriving the DER each time.
func parseLeaf(cert tls.Certificate) (*x509.Certificate, error) {
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("link: certificate has no DER")
	}
	return x509.ParseCertificate(cert.Certificate[0])
}
