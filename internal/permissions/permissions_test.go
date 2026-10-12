package permissions

import (
	"errors"
	"slices"
	"testing"
)

func TestPresets(t *testing.T) {
	tests := []struct {
		preset Preset
		want   []Permission
	}{
		{PresetAsker, []Permission{PermResourcesView}},
		{PresetBuilder, []Permission{PermResourcesView, PermResourcesEdit, PermAgentsEdit, PermMembersView}},
		{PresetManager, []Permission{PermResourcesView, PermResourcesEdit, PermAgentsEdit, PermChatsView, PermMembersView, PermMembersEdit}},
		{PresetAdmin, all},
		{"unknown", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.preset), func(t *testing.T) {
			if got := tt.preset.Permissions().List(); !slices.Equal(got, tt.want) {
				t.Errorf("Permissions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPresetsNest(t *testing.T) {
	list := Presets()
	for i := 1; i < len(list); i++ {
		smaller, larger := list[i-1].Permissions(), list[i].Permissions()
		if !larger.HasAll(smaller) {
			t.Errorf("%s does not contain %s", list[i], list[i-1])
		}
		if len(larger) <= len(smaller) {
			t.Errorf("%s adds nothing to %s", list[i], list[i-1])
		}
	}
}

func TestPresetsObeyRequires(t *testing.T) {
	for _, p := range Presets() {
		set := p.Permissions()
		for perm := range set {
			for _, need := range requires[perm] {
				if !set.Has(need) {
					t.Errorf("%s has %s but not %s, which it requires", p, perm, need)
				}
			}
		}
	}
}

func TestPresetPermissionsAreCopies(t *testing.T) {
	a := PresetAsker.Permissions()
	a[PermProjectEdit] = true
	if PresetAsker.Permissions().Has(PermProjectEdit) {
		t.Error("changing a returned set changed the preset")
	}
}

func TestGrant(t *testing.T) {
	asker := PresetAsker.Permissions
	builder := PresetBuilder.Permissions
	manager := PresetManager.Permissions
	admin := PresetAdmin.Permissions

	tests := []struct {
		name    string
		granter Set
		target  Set
		perm    Permission
		wantErr error // nil means the grant must work
	}{
		{"admin gives agents:edit to an asker", admin(), asker(), PermAgentsEdit, nil},
		{"admin makes a builder a manager", admin(), builder(), PermMembersEdit, nil},
		{"admin grants to a member with no permissions", admin(), Set{}, PermResourcesView, nil},
		{"manager gives members:view to an asker", manager(), asker(), PermMembersView, nil},
		{"owner grants project:edit", Effective(true, nil), manager(), PermProjectEdit, nil},
		{"builder cannot grant", builder(), asker(), PermAgentsEdit, ErrNotAllowed},
		{"member with no permissions cannot grant", Set{}, Set{}, PermResourcesView, ErrNotAllowed},
		{"manager cannot grant what they lack", manager(), manager(), PermProjectEdit, ErrNotAllowed},
		{"resources:edit needs resources:view", admin(), Set{}, PermResourcesEdit, ErrMissingRequirement},
		{"agents:edit needs resources:view", admin(), Set{PermMembersView: true}, PermAgentsEdit, ErrMissingRequirement},
		{"members:edit needs agents:edit", admin(), Set{PermResourcesView: true, PermResourcesEdit: true, PermMembersView: true}, PermMembersEdit, ErrMissingRequirement},
		{"project:edit needs members:edit", admin(), builder(), PermProjectEdit, ErrMissingRequirement},
		{"unknown permission", admin(), builder(), "agents:view", ErrUnknownPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Grant(tt.granter, tt.target, tt.perm)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Grant() error = %v, want %v", err, tt.wantErr)
			}
			// Success adds the permission; failure changes nothing.
			if got, want := tt.target.Has(tt.perm), tt.wantErr == nil; got != want {
				t.Errorf("target.Has(%s) = %v, want %v", tt.perm, got, want)
			}
		})
	}
}

func TestCanInvite(t *testing.T) {
	tests := []struct {
		name    string
		granter Set
		preset  Preset
		want    bool
	}{
		{"manager invites an asker", PresetManager.Permissions(), PresetAsker, true},
		{"manager invites a manager", PresetManager.Permissions(), PresetManager, true},
		{"manager cannot invite an admin", PresetManager.Permissions(), PresetAdmin, false},
		{"builder cannot invite", PresetBuilder.Permissions(), PresetAsker, false},
		{"admin invites an admin", PresetAdmin.Permissions(), PresetAdmin, true},
		{"owner invites an admin", Effective(true, nil), PresetAdmin, true},
		{"unknown preset", PresetAdmin.Permissions(), "owner", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanInvite(tt.granter, tt.preset); got != tt.want {
				t.Errorf("CanInvite() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	tests := []struct {
		name    string
		isOwner bool
		held    Set
		want    []Permission
	}{
		{"owner without membership", true, nil, all},
		{"owner as an asker", true, PresetAsker.Permissions(), all},
		{"asker", false, PresetAsker.Permissions(), []Permission{PermResourcesView}},
		{"member with no permissions", false, Set{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Effective(tt.isOwner, tt.held).List(); !slices.Equal(got, tt.want) {
				t.Errorf("Effective() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseSet(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		want    []Permission
		wantErr error
	}{
		{"empty", nil, nil, nil},
		{"known", []string{"members:view", "resources:view"}, []Permission{PermResourcesView, PermMembersView}, nil},
		{"dropped permission", []string{"agents:view"}, nil, ErrUnknownPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSet(tt.names)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseSet() error = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got.List(), tt.want) {
				t.Errorf("ParseSet() = %v, want %v", got.List(), tt.want)
			}
		})
	}
}
