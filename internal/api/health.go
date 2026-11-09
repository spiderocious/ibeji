package api

import (
	"context"
	"net/http"
	"runtime"
	"time"
)

// A health view for the pair.
//
// Until this existed, answering "are both daemons up?" meant running `ibeji
// doctor` on one machine, walking to the other, and running it again — and
// when the two could not see each other, neither report told you whether the
// far end was even alive. One place that says what both ends look like, from
// here, is what a person actually wants when something is wrong.

// HealthState is what the health view needs from the daemon.
type HealthState interface {
	RemoteState
	Self() SelfInfo
	Paired() (*Device, bool)
	// HealthChecks is this machine's doctor run, flattened. Returned as a
	// slice rather than behind another interface: the daemon already owns
	// doctor, and one more abstraction here would buy nothing.
	HealthChecks() []HealthCheck
}

// HealthCheck is one check, flattened for the wire.
type HealthCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
}

// HealthSide is one machine's state.
type HealthSide struct {
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	OS          string        `json:"os,omitempty"`
	Arch        string        `json:"arch,omitempty"`
	Addresses   []string      `json:"addresses,omitempty"`
	Port        int           `json:"port,omitempty"`
	Reachable   bool          `json:"reachable"`
	Unreachable string        `json:"unreachable,omitempty"`
	Checks      []HealthCheck `json:"checks,omitempty"`
}

// RegisterHealthRoutes wires the health view.
func RegisterHealthRoutes(s *Server, state HealthState) {
	s.Handle(Route{
		Method: "GET", Path: "/health",
		Summary: "Both daemons at a glance: versions, reachability and checks",
	}, func(w http.ResponseWriter, r *http.Request) {
		self := state.Self()

		here := HealthSide{
			Name:      self.Name,
			Version:   self.Version,
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			Addresses: self.Addresses,
			Port:      self.Port,
			Reachable: true,
		}
		here.Checks = state.HealthChecks()

		resp := map[string]any{
			"at":   time.Now(),
			"here": here,
		}

		paired, ok := state.Paired()
		if !ok || paired == nil {
			resp["paired"] = false
			WriteJSON(w, resp)
			return
		}
		resp["paired"] = true

		there := HealthSide{
			Name:      paired.Name,
			Reachable: paired.Status == StatusLive,
		}
		// Ask the far machine about itself, rather than reporting what we
		// last heard. "Reachable" has to mean reachable now.
		if client, err := state.Remote(); err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			if info, err := client.Info(ctx); err == nil {
				there.OS = info.OS
				there.Arch = info.Arch
				// The far daemon reports its toolchain, and its own version
				// rides along there when it is new enough to send it.
				if v := info.Toolchain["ibeji"]; v != "" {
					there.Version = v
				}
				there.Reachable = true
			} else {
				there.Reachable = false
				there.Unreachable = err.Error()
			}
		} else {
			there.Reachable = false
			there.Unreachable = err.Error()
		}
		if paired.Address != "" {
			there.Addresses = []string{paired.Address}
		}
		resp["there"] = there

		WriteJSON(w, resp)
	})
}
