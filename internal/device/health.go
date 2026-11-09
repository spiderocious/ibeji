package device

import (
	"github.com/feranmi/ibeji/internal/api"
	"github.com/feranmi/ibeji/internal/doctor"
)

// HealthChecks implements api.HealthState.
//
// The same checks `ibeji doctor` runs, flattened for the health view, so the
// two can never disagree about what is wrong with this machine.
func (d *Daemon) HealthChecks() []api.HealthCheck {
	rep := d.Doctor()
	out := make([]api.HealthCheck, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		out = append(out, api.HealthCheck{
			Name:     c.Name,
			OK:       c.OK,
			Severity: severityName(c.Severity),
			Detail:   c.Detail,
			Fix:      c.Fix,
		})
	}
	return out
}

func severityName(s doctor.Severity) string {
	switch s {
	case doctor.Pass:
		return "pass"
	case doctor.Warn:
		return "warn"
	default:
		return "fail"
	}
}
