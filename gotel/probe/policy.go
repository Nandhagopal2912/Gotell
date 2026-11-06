package probe

// StatusPolicy decides which HTTP status codes count as healthy.
// Min and Max are inclusive.
type StatusPolicy struct {
	Min int
	Max int
}

// Default2xx accepts any 2xx status.
var Default2xx = StatusPolicy{Min: 200, Max: 299}

// Allows reports whether code is a healthy status under this policy
func (sp StatusPolicy) Allows(code int) bool {
	return code >= sp.Min && code <= sp.Max
}

// Healthy reports whether r is a healthy result under the policy sp
func (sp StatusPolicy) Healthy(r Result) bool {
	return r.Err == nil && sp.Allows(r.StatusCode)
}
