// Package permissions models what a member may do in a project, and who
// may grant which permission to whom.
//
// Membership itself lets a person chat with the project's agents and see
// every agent's name and description. Everything else is a permission.
package permissions

import (
	"errors"
	"fmt"
	"slices"
)

// Permission is one thing a member may do in a project.
// It is always written <area>:view or <area>:edit.
type Permission string

// The permissions a member can hold.
const (
	PermResourcesView Permission = "resources:view" // read the project's resources
	PermResourcesEdit Permission = "resources:edit" // add or remove resources
	PermAgentsEdit    Permission = "agents:edit"    // create and edit agents, including prompts
	PermMembersView   Permission = "members:view"   // see who is in the project
	PermMembersEdit   Permission = "members:edit"   // invite and remove members, change their permissions
	PermProjectEdit   Permission = "project:edit"   // rename the project, change its settings
)

// all lists every permission in display order.
var all = []Permission{
	PermResourcesView,
	PermResourcesEdit,
	PermAgentsEdit,
	PermMembersView,
	PermMembersEdit,
	PermProjectEdit,
}

// requires lists, for each permission, what the target must already hold
// before Grant gives it. It is unexported, so no other package can change
// the rules.
var requires = map[Permission][]Permission{
	PermResourcesEdit: {PermResourcesView},
	PermAgentsEdit:    {PermResourcesView},
	PermMembersEdit:   {PermResourcesEdit, PermAgentsEdit, PermMembersView},
	PermProjectEdit:   {PermMembersEdit},
}

// ErrUnknownPermission is returned for a string that names no permission.
var ErrUnknownPermission = errors.New("unknown permission")

// Parse returns the permission named s.
func Parse(s string) (Permission, error) {
	p := Permission(s)
	if !slices.Contains(all, p) {
		return "", fmt.Errorf("%q: %w", s, ErrUnknownPermission)
	}
	return p, nil
}

// Set is the set of permissions one member holds in one project.
// A nil Set is empty and safe to read.
type Set map[Permission]bool

// All returns a new set with every permission.
func All() Set {
	s := Set{}
	for _, p := range all {
		s[p] = true
	}
	return s
}

// ParseSet builds a set from permission names, such as rows read from the
// database. One unknown name fails the whole set.
func ParseSet(names []string) (Set, error) {
	s := Set{}
	for _, name := range names {
		p, err := Parse(name)
		if err != nil {
			return nil, err
		}
		s[p] = true
	}
	return s, nil
}

// Has reports whether the set holds p.
func (s Set) Has(p Permission) bool {
	return s[p]
}

// HasAll reports whether the set holds every permission in other.
func (s Set) HasAll(other Set) bool {
	for p, ok := range other {
		if ok && !s[p] {
			return false
		}
	}
	return true
}

// List returns the permissions in the set, in display order.
func (s Set) List() []Permission {
	var list []Permission
	for _, p := range all {
		if s[p] {
			list = append(list, p)
		}
	}
	return list
}

// Strings returns the permission names in the set, in display order.
// The store saves them in this form.
func (s Set) Strings() []string {
	var names []string
	for _, p := range s.List() {
		names = append(names, string(p))
	}
	return names
}

// Effective returns the permissions an account has in a project.
// The instance owner has every permission in every project, even without
// a membership. Anyone else has what their membership holds.
func Effective(isOwner bool, held Set) Set {
	if isOwner {
		return All()
	}
	return held
}
