package permissions

import (
	"errors"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		set     Set
		wantErr string // empty for a valid set
	}{
		{"empty", Set{}, ""},
		{"every preset", PresetAdmin.Permissions(), ""},
		{"edit without view", Set{PermResourcesEdit: true}, "resources:edit needs resources:view"},
		{"chats without resources", Set{PermChatsView: true}, "chats:view needs resources:view"},
		{"members:edit without agents:edit", Set{PermResourcesView: true, PermResourcesEdit: true, PermMembersView: true, PermMembersEdit: true}, "members:edit needs agents:edit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.set)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidSet) {
				t.Fatalf("Validate() = %v, want ErrInvalidSet", err)
			}
			if got := err.Error(); got != tt.wantErr+": invalid permission set" {
				t.Errorf("Validate() = %q, want it to start with %q", got, tt.wantErr)
			}
		})
	}
}

func TestPresetsAreValid(t *testing.T) {
	for _, p := range Presets() {
		if err := Validate(p.Permissions()); err != nil {
			t.Errorf("preset %s: %v", p, err)
		}
	}
}

func TestCanManageAndSet(t *testing.T) {
	asker, builder := PresetAsker.Permissions(), PresetBuilder.Permissions()
	manager, admin := PresetManager.Permissions(), PresetAdmin.Permissions()
	owner := Effective(true, nil)

	tests := []struct {
		name           string
		editor, target Set
		next           Set
		wantManage     bool
		wantSet        bool
	}{
		{"manager raises an asker to builder", manager, asker, builder, true, true},
		{"manager lowers another manager", manager, manager, asker, true, true},
		{"manager cannot touch an admin", manager, admin, asker, false, false},
		{"manager cannot give project:edit", manager, asker, admin, true, false},
		{"builder cannot manage", builder, asker, asker, false, false},
		{"owner manages an admin", owner, admin, asker, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanManage(tt.editor, tt.target); got != tt.wantManage {
				t.Errorf("CanManage() = %v, want %v", got, tt.wantManage)
			}
			if got := CanSet(tt.editor, tt.target, tt.next); got != tt.wantSet {
				t.Errorf("CanSet() = %v, want %v", got, tt.wantSet)
			}
		})
	}
}
