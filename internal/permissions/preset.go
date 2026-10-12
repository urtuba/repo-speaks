package permissions

// Preset is a named starting permission set. Each preset contains the one
// before it: asker, builder, manager, admin.
type Preset string

// The presets, from the smallest to the largest.
const (
	PresetAsker   Preset = "asker"   // views and asks questions, changes nothing
	PresetBuilder Preset = "builder" // manages resources and agents
	PresetManager Preset = "manager" // a builder who can also manage members
	PresetAdmin   Preset = "admin"   // a manager who can also edit the project
)

// presets lists each preset with what it adds to the one before it.
var presets = []struct {
	preset Preset
	adds   []Permission
}{
	{PresetAsker, []Permission{PermResourcesView}},
	{PresetBuilder, []Permission{PermResourcesEdit, PermAgentsEdit, PermMembersView}},
	{PresetManager, []Permission{PermMembersEdit}},
	{PresetAdmin, []Permission{PermProjectEdit}},
}

// Presets returns every preset, from the smallest to the largest.
func Presets() []Preset {
	list := make([]Preset, len(presets))
	for i, p := range presets {
		list[i] = p.preset
	}
	return list
}

// Permissions returns a new set with the preset's permissions.
// An unknown preset returns an empty set.
func (p Preset) Permissions() Set {
	s := Set{}
	for _, step := range presets {
		for _, perm := range step.adds {
			s[perm] = true
		}
		if step.preset == p {
			return s
		}
	}
	return Set{}
}

// valid reports whether p is one of the presets.
func (p Preset) valid() bool {
	for _, step := range presets {
		if step.preset == p {
			return true
		}
	}
	return false
}
