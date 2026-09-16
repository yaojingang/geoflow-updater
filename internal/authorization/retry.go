package authorization

import "path/filepath"

// RetryAfter reveals only the bounded delay, without exposing factor state.
func (s Service) RetryAfter(id string, scope Scope) int {
	dir, err := s.instanceDir(id)
	if err != nil || !validScope(scope) {
		return int(maxLockoutSeconds)
	}
	seconds := int64(1)
	for _, name := range []string{"mutation.attempts", "mutation." + string(scope) + ".attempts"} {
		state, err := readAttemptState(filepath.Join(dir, name))
		if err != nil {
			return int(maxLockoutSeconds)
		}
		if remaining := state.LockedUntil - s.now().Unix(); remaining > seconds {
			seconds = remaining
		}
	}
	if seconds > maxLockoutSeconds {
		seconds = maxLockoutSeconds
	}
	return int(seconds)
}
