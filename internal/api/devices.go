package api

import (
	"net/http"
	"runtime"
	"time"

	"github.com/feranmi/ibeji/internal/discovery"
	"github.com/feranmi/ibeji/internal/identity"
	"github.com/feranmi/ibeji/internal/link"
)

// DeviceStatus is what the device row shows: live or unreachable.
//
// Offline is normal on a LAN — the other machine is asleep — so it is a status,
// never an error, and the UI never shakes or pulses it.
type DeviceStatus string

const (
	StatusLive        DeviceStatus = "live"
	StatusUnreachable DeviceStatus = "unreachable"
)

// Mode is the permission mode, set on the receiving device, per direction.
type Mode string

const (
	ModePrompt Mode = "prompt"
	ModeBypass Mode = "bypass"
)

// Device is a paired device as the API reports it.
//
// Address is never omitted. The PRD requires IPs to be plainly visible and the
// design system makes Device.Address a required slot, so a response that left
// it out would make a compliant device row impossible to render.
type Device struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Address     string       `json:"address"`
	Status      DeviceStatus `json:"status"`
	Mode        Mode         `json:"mode"`
	Fingerprint string       `json:"fingerprint"`
	PairedAt    *time.Time   `json:"pairedAt,omitempty"`
	LastSeen    *time.Time   `json:"lastSeen,omitempty"`
}

// SelfInfo is this device's own view of itself.
type SelfInfo struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Paired      bool   `json:"paired"`

	// Addresses are this machine's LAN addresses, best first, and Port is
	// where the peer listener is bound. The window shows them so the other
	// Mac has something to type when mDNS is blocked — asking for an IP the
	// product never displays sends people to System Settings to find it.
	Addresses []string `json:"addresses"`
	Port      int      `json:"port"`
}

// Candidate is a device seen on the LAN and not yet paired with.
type Candidate struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
	APIVersion  string `json:"apiVersion"`
	Compatible  bool   `json:"compatible"`
	Source      string `json:"source"`
}

// PairingView is an in-flight pairing attempt, as the ceremony screen shows it.
type PairingView struct {
	ID              string    `json:"id"`
	Code            string    `json:"code"`
	CodeFormatted   string    `json:"codeFormatted"`
	PeerName        string    `json:"peerName"`
	PeerAddress     string    `json:"peerAddress"`
	PeerFingerprint string    `json:"peerFingerprint"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// DeviceState is what the daemon knows about devices. The API reads it; the
// daemon owns it.
//
// An interface so the API's handlers are testable without a daemon, and so the
// daemon can evolve its internals without touching the HTTP layer.
type DeviceState interface {
	Self() SelfInfo
	Paired() (*Device, bool)
	Candidates() []Candidate

	Rename(name string) error
	SetMode(deviceID string, mode Mode) error
	Unpair(deviceID string) error

	// PendingPairing reports a ceremony in flight, so a window opened or
	// reopened mid-pairing shows the code rather than nothing.
	PendingPairing() *PairingView
	BeginPairing(address string) (*PairingView, error)
	ConfirmPairing(sessionID string) (*Device, error)
	CancelPairing(sessionID string) error
}

// RegisterDeviceRoutes wires the device and pairing endpoints.
func (s *Server) RegisterDeviceRoutes(state DeviceState) {
	s.Handle(Route{
		Method: "GET", Path: "/self",
		Summary: "This device's name, fingerprint, version and pairing status",
	}, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, state.Self())
	})

	s.Handle(Route{
		Method: "GET", Path: "/devices",
		Summary: "The paired device: name, address, status and permission mode",
	}, func(w http.ResponseWriter, r *http.Request) {
		devices := []Device{}
		if d, ok := state.Paired(); ok {
			devices = append(devices, *d)
		}
		WriteJSON(w, map[string]any{"devices": devices})
	})

	s.Handle(Route{
		Method: "GET", Path: "/discovery",
		Summary: "Devices seen on the LAN that are not yet paired",
	}, func(w http.ResponseWriter, r *http.Request) {
		c := state.Candidates()
		if c == nil {
			c = []Candidate{}
		}
		// The hint explains an empty list, and why. A silent network and a
		// network where the other machine is simply off need different advice.
		WriteJSON(w, map[string]any{
			"candidates": c,
			"hint":       discovery.EmptyStateHint(true, len(c) > 0),
		})
	})

	s.Handle(Route{
		Method: "POST", Path: "/self/name", Mutating: true,
		Summary: "Rename this device; the name propagates to the peer",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if err := validateDeviceName(req.Name); err != nil {
			WriteError(w, err)
			return
		}
		if err := state.Rename(req.Name); err != nil {
			WriteError(w, err)
			return
		}
		s.events.Publish(EventDeviceChanged, state.Self())
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/devices/mode", Mutating: true,
		Summary: "Set a paired device's permission mode (prompt or bypass)",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeviceID string `json:"deviceId"`
			Mode     Mode   `json:"mode"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Mode != ModePrompt && req.Mode != ModeBypass {
			WriteError(w, Errf(CodeBadRequest, "mode must be %q or %q, got %q",
				ModePrompt, ModeBypass, req.Mode))
			return
		}
		if err := state.SetMode(req.DeviceID, req.Mode); err != nil {
			WriteError(w, err)
			return
		}
		if d, ok := state.Paired(); ok {
			s.events.Publish(EventDeviceChanged, d)
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "POST", Path: "/unpair", Mutating: true,
		Summary: "Unpair: revoke the pinned key and close every tunnel, sync and session",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeviceID string `json:"deviceId"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if err := state.Unpair(req.DeviceID); err != nil {
			WriteError(w, err)
			return
		}
		s.events.Publish(EventDeviceChanged, nil)
		WriteJSON(w, nil)
	})

	// --- pairing ---

	s.Handle(Route{
		Method: "GET", Path: "/pair",
		Summary: "The pairing ceremony in flight, if any",
	}, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, map[string]any{"pairing": state.PendingPairing()})
	})

	s.Handle(Route{
		Method: "POST", Path: "/pair/start", Mutating: true,
		Summary: "Begin pairing with a device at an address; returns the six-digit code",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Address string `json:"address"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if req.Address == "" {
			WriteError(w, Errf(CodeBadRequest, "address is required"))
			return
		}
		view, err := state.BeginPairing(req.Address)
		if err != nil {
			WriteError(w, err)
			return
		}
		s.events.Publish(EventPairingStarted, view)
		WriteJSON(w, view)
	})

	s.Handle(Route{
		Method: "POST", Path: "/pair/confirm", Mutating: true,
		Summary: "Confirm the codes matched; pins the peer's key",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"sessionId"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		device, err := state.ConfirmPairing(req.SessionID)
		if err != nil {
			WriteError(w, err)
			return
		}
		s.events.Publish(EventPairingDone, device)
		WriteJSON(w, map[string]any{"device": device})
	})

	s.Handle(Route{
		Method: "POST", Path: "/pair/cancel", Mutating: true,
		Summary: "Abandon a pairing attempt",
	}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"sessionId"`
		}
		if err := DecodeBody(r, &req); err != nil {
			WriteError(w, err)
			return
		}
		if err := state.CancelPairing(req.SessionID); err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, nil)
	})

	s.Handle(Route{
		Method: "GET", Path: "/events",
		Summary: "Server-sent stream of daemon events",
	}, s.HandleEvents)
}

// maxDeviceNameLen bounds a name so it cannot break an mDNS TXT record or a
// device row's layout.
const maxDeviceNameLen = 63

// validateDeviceName rejects names that would not survive the wire or the UI.
func validateDeviceName(name string) error {
	switch {
	case name == "":
		return Errf(CodeBadRequest, "a device name cannot be empty")
	case len(name) > maxDeviceNameLen:
		return Errf(CodeBadRequest, "a device name must be %d characters or fewer", maxDeviceNameLen)
	}
	for _, r := range name {
		// Control characters would corrupt the TXT record and could smuggle
		// escape sequences into a terminal rendering the device list.
		if r < 0x20 || r == 0x7f {
			return Errf(CodeBadRequest, "a device name cannot contain control characters")
		}
	}
	return nil
}

// --- conversions from the daemon's types ---

// DeviceFrom builds the API view of a pinned peer.
func DeviceFrom(p *identity.Peer, mode Mode, online bool) *Device {
	if p == nil {
		return nil
	}
	d := &Device{
		ID:          identity.Fingerprint(p.PublicKey),
		Name:        p.Name,
		Address:     p.LastAddress,
		Status:      StatusUnreachable,
		Mode:        mode,
		Fingerprint: identity.Fingerprint(p.PublicKey),
	}
	if online {
		d.Status = StatusLive
	}
	if !p.PairedAt.IsZero() {
		t := p.PairedAt
		d.PairedAt = &t
	}
	if !p.LastSeen.IsZero() {
		t := p.LastSeen
		d.LastSeen = &t
	}
	return d
}

// CandidateFrom builds the API view of a discovered device.
func CandidateFrom(c discovery.Candidate) Candidate {
	return Candidate{
		Name:        c.Name,
		Address:     c.Address,
		Port:        c.Port,
		Fingerprint: c.Fingerprint(),
		APIVersion:  c.APIVersion,
		Compatible:  c.Compatible(),
		Source:      string(c.Source),
	}
}

// PairingViewFrom builds the API view of an in-flight pairing session.
func PairingViewFrom(s *link.PairingSession) *PairingView {
	if s == nil {
		return nil
	}
	return &PairingView{
		ID:              s.ID,
		Code:            s.Code,
		CodeFormatted:   link.FormatCode(s.Code),
		PeerName:        s.Peer.DeviceName,
		PeerAddress:     s.Peer.Address,
		PeerFingerprint: s.Fingerprint(),
		ExpiresAt:       s.ExpiresAt,
	}
}

// SelfInfoFrom builds this device's own view.
//
// peerPort is where the peer listener is bound; zero when it is not running
// yet, in which case the window shows the address without a port rather than
// a misleading ":0".
func SelfInfoFrom(store *identity.Store, version string, peerPort int) SelfInfo {
	return SelfInfo{
		Name:        store.Name(),
		Fingerprint: store.Self().Fingerprint(),
		Version:     version,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Paired:      store.Paired(),
		Addresses:   discovery.LocalAddresses(),
		Port:        peerPort,
	}
}
