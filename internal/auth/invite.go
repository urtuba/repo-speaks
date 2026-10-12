package auth

import "time"

// InviteLifetime is how long an invite link works if nobody uses it.
const InviteLifetime = 7 * 24 * time.Hour

// InviteExpiry returns when an invite made at now stops working.
func InviteExpiry(now time.Time) time.Time {
	return now.Add(InviteLifetime)
}
