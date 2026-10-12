package permissions

import (
	"errors"
	"fmt"
)

// ErrInvalidSet is returned by Validate for a set that holds a
// permission without one it requires.
var ErrInvalidSet = errors.New("invalid permission set")

// List returns every permission in display order.
func List() []Permission {
	return append([]Permission(nil), all...)
}

// Requires returns what p needs, in display order. The page uses it to
// tick and lock those permissions when p is ticked.
func Requires(p Permission) []Permission {
	return append([]Permission(nil), requires[p]...)
}

// Validate checks that every permission in s comes with the permissions
// it requires. The error names the first gap, such as
// "resources:edit needs resources:view".
func Validate(s Set) error {
	for _, p := range s.List() {
		for _, need := range requires[p] {
			if !s.Has(need) {
				return fmt.Errorf("%s needs %s: %w", p, need, ErrInvalidSet)
			}
		}
	}
	return nil
}

// CanManage reports whether editor may change or remove a member who
// holds target. The editor needs members:edit and must hold everything
// the member holds, so nobody can push out someone above them.
func CanManage(editor, target Set) bool {
	return editor.Has(PermMembersEdit) && editor.HasAll(target)
}

// CanSet reports whether editor may change a member's permissions from
// target to next: the editor may manage the member, and next stays
// within the editor's own permissions. It does not check that next is
// valid; use Validate for that.
func CanSet(editor, target, next Set) bool {
	return CanManage(editor, target) && editor.HasAll(next)
}
