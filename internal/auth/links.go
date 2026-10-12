package auth

import "time"

// InviteLifetime is how long an invite link works if nobody uses it.
const InviteLifetime = 7 * 24 * time.Hour

// InviteExpiry returns when an invite made at now stops working.
func InviteExpiry(now time.Time) time.Time {
	return now.Add(InviteLifetime)
}

// ResetLifetime is how long a password reset link works. A reset is
// urgent, so the link is short-lived.
const ResetLifetime = 24 * time.Hour

// ResetExpiry returns when a reset link made at now stops working.
func ResetExpiry(now time.Time) time.Time {
	return now.Add(ResetLifetime)
}
