package permissions

import (
	"errors"
	"fmt"
)

// Errors returned by Grant. Check them with errors.Is.
var (
	ErrNotAllowed         = errors.New("not allowed")
	ErrMissingRequirement = errors.New("missing requirement")
)

// Grant adds perm to target on behalf of granter. Both sets belong to
// members of the same project, and target must not be nil.
//
// The granter must hold members:edit, and must hold perm itself: nobody
// can grant what they do not have. The target must already hold
// everything perm requires. On error, target does not change.
func Grant(granter, target Set, perm Permission) error {
	if _, err := Parse(string(perm)); err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	if !granter.Has(PermMembersEdit) {
		return fmt.Errorf("grant %s: granter lacks %s: %w", perm, PermMembersEdit, ErrNotAllowed)
	}
	if !granter.Has(perm) {
		return fmt.Errorf("grant %s: granter does not hold it: %w", perm, ErrNotAllowed)
	}
	for _, need := range requires[perm] {
		if !target.Has(need) {
			return fmt.Errorf("grant %s: target lacks %s: %w", perm, need, ErrMissingRequirement)
		}
	}
	target[perm] = true
	return nil
}

// CanInvite reports whether granter may invite someone with preset p.
// New members always join through an invite, so this is the only way
// a member starts with permissions. The granter must hold members:edit
// and every permission in the preset.
func CanInvite(granter Set, p Preset) bool {
	return p.valid() && granter.Has(PermMembersEdit) && granter.HasAll(p.Permissions())
}
