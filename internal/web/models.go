package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/urtuba/repo-speaks/internal/llm"
	"github.com/urtuba/repo-speaks/internal/store"
)

// Models live on the settings page and need project:edit. A model's key
// is write-only: no page or form holds it, only its last 4 characters.
// Saving a model asks the host for its model list, so a typo in the
// model ID fails on save and not when a question runs.

// modelNotices are the messages the models card shows after an action,
// by the name in the URL. A name that is not here shows nothing, so the
// URL cannot put text on the page.
var modelNotices = map[string]string{
	"model-added":   "Model added.",
	"model-deleted": "Model deleted.",
	"default":       "Default model saved.",
}

// Limits on a model's fields.
const (
	maxModelName   = 100
	maxModelID     = 200
	maxModelAPIKey = 500
)

// apiKeyRE matches an API key: visible ASCII, no spaces. Hosts use
// letters, digits and a few signs; anything else would break the
// Authorization header.
var apiKeyRE = regexp.MustCompile(`^[\x21-\x7e]+$`)

// modelForm is the add and edit form, checked.
type modelForm struct {
	Name    string
	BaseURL string // normalized, with no trailing slash
	ModelID string
	APIKey  string // empty: none on add, keep the current one on edit
}

// parseModelForm reads and checks the model form. msg explains a
// problem, or is empty. It does not call the host.
func parseModelForm(r *http.Request) (f modelForm, msg string) {
	f.Name = strings.TrimSpace(r.PostFormValue("name"))
	if n := utf8.RuneCountInString(f.Name); n == 0 || n > maxModelName {
		return f, "A model name must be 1 to 100 characters."
	}
	base, err := llm.ParseBaseURL(r.PostFormValue("base_url"))
	if err != nil {
		return f, "Not saved: " + err.Error() + "."
	}
	f.BaseURL = base
	f.ModelID = strings.TrimSpace(r.PostFormValue("model_id"))
	if n := utf8.RuneCountInString(f.ModelID); n == 0 || n > maxModelID {
		return f, "A model ID must be 1 to 200 characters, such as z-ai/glm-5.3-flash."
	}
	if strings.ContainsFunc(f.ModelID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return f, "A model ID may not contain spaces."
	}
	f.APIKey = strings.TrimSpace(r.PostFormValue("api_key"))
	if f.APIKey != "" && (len(f.APIKey) > maxModelAPIKey || !apiKeyRE.MatchString(f.APIKey)) {
		return f, "That does not look like an API key: it should be up to 500 letters, digits and signs, with no spaces."
	}
	return f, ""
}

// checkFailed is the message for a model ID the host check refused. The
// llm package's errors are safe to show.
func checkFailed(err error) string {
	return "Not saved. Checking the model ID failed: " + err.Error()
}

// addModel adds a model to the project, after the host confirms the
// model ID. It needs project:edit.
func (s *server) addModel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	// The form is shown again after an error, without the key.
	page := newSettingsPage(r, p)
	page.ModelName = r.PostFormValue("name")
	page.ModelBaseURL = r.PostFormValue("base_url")
	page.ModelID = r.PostFormValue("model_id")
	f, msg := parseModelForm(r)
	if msg != "" {
		page.ModelError = msg
		s.renderSettings(w, r, http.StatusBadRequest, page)
		return
	}
	if err := s.llm.CheckModel(r.Context(), f.BaseURL, f.APIKey, f.ModelID); err != nil {
		page.ModelError = checkFailed(err)
		s.renderSettings(w, r, http.StatusBadRequest, page)
		return
	}
	_, err := s.store.AddModel(r.Context(), store.NewModel{
		ProjectID: p.ID, Name: f.Name, BaseURL: f.BaseURL, ModelID: f.ModelID, APIKey: f.APIKey,
	}, s.now())
	if errors.Is(err, store.ErrModelExists) {
		page.ModelError = "The project already has a model with this name."
		s.renderSettings(w, r, http.StatusConflict, page)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=model-added#models", http.StatusSeeOther)
}

// openModel loads the project and the model in the URL, and checks that
// the viewer has project:edit. A model of another project gets 404, like
// a missing one. It returns false when it has already answered.
func (s *server) openModel(w http.ResponseWriter, r *http.Request) (store.Project, store.Model, bool) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return store.Project{}, store.Model{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("model"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Project{}, store.Model{}, false
	}
	m, err := s.store.Model(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Project{}, store.Model{}, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return store.Project{}, store.Model{}, false
	}
	return p, m, true
}

// modelPage is the data for model.html.
type modelPage struct {
	User    *store.User
	Project store.Project
	Model   store.Model
	// The form's fields, as sent when there is an error.
	Name    string
	BaseURL string
	ModelID string
	Saved   string // "1" after a save, "cleared" when the save cleared the key
	Error   string
	Test    *modelTest // the Test button's result, without htmx
}

// newModelPage fills the model page with the model's values.
func newModelPage(r *http.Request, p store.Project, m store.Model) modelPage {
	return modelPage{
		User: &currentSession(r).user, Project: p, Model: m,
		Name: m.Name, BaseURL: m.BaseURL, ModelID: m.ModelID,
	}
}

// modelPageView shows a model with a form to change it. The key itself
// is never shown.
func (s *server) modelPageView(w http.ResponseWriter, r *http.Request) {
	p, m, ok := s.openModel(w, r)
	if !ok {
		return
	}
	page := newModelPage(r, p, m)
	if saved := r.URL.Query().Get("saved"); saved == "1" || saved == "cleared" {
		page.Saved = saved
	}
	s.render(w, http.StatusOK, "model.html", page)
}

// saveModel changes a model, after the host confirms the model ID. An
// empty key field keeps the key, unless the base URL changes: then the
// key is cleared, so it never goes to a host it was not entered for.
func (s *server) saveModel(w http.ResponseWriter, r *http.Request) {
	p, m, ok := s.openModel(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	page := newModelPage(r, p, m)
	page.Name = r.PostFormValue("name")
	page.BaseURL = r.PostFormValue("base_url")
	page.ModelID = r.PostFormValue("model_id")
	f, msg := parseModelForm(r)
	if msg != "" {
		page.Error = msg
		s.render(w, http.StatusBadRequest, "model.html", page)
		return
	}
	cur, err := s.store.ModelEndpoint(r.Context(), p.ID, m.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The check uses the key the model will have after the save.
	key := f.APIKey
	urlChanged := f.BaseURL != cur.BaseURL
	if key == "" && !urlChanged {
		key = cur.APIKey
	}
	if err := s.llm.CheckModel(r.Context(), f.BaseURL, key, f.ModelID); err != nil {
		page.Error = checkFailed(err)
		s.render(w, http.StatusBadRequest, "model.html", page)
		return
	}
	err = s.store.UpdateModel(r.Context(), p.ID, m.ID, store.ModelChange{
		Name: f.Name, BaseURL: f.BaseURL, ModelID: f.ModelID, APIKey: f.APIKey,
	}, s.now())
	if errors.Is(err, store.ErrModelExists) {
		page.Error = "The project already has a model with this name."
		s.render(w, http.StatusConflict, "model.html", page)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r) // deleted while the host was checked
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	saved := "1"
	if urlChanged && f.APIKey == "" && cur.APIKey != "" {
		saved = "cleared"
	}
	http.Redirect(w, r, modelPath(p.ID, m.ID)+"?saved="+saved, http.StatusSeeOther)
}

// modelTest is the data for the "model_test" template: the result of
// the Test button.
type modelTest struct {
	Name  string // the model's name
	OK    bool
	Error string // why the test failed; safe to show
}

// testModel sends the model one short request with one tool, and shows
// whether it called the tool. htmx swaps the result into the page. A
// browser without JavaScript gets the model page with the result.
func (s *server) testModel(w http.ResponseWriter, r *http.Request) {
	p, m, ok := s.openModel(w, r)
	if !ok {
		return
	}
	e, err := s.store.ModelEndpoint(r.Context(), p.ID, m.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	result := &modelTest{Name: m.Name, OK: true}
	if err := s.llm.Test(r.Context(), e.BaseURL, e.APIKey, e.ModelID); err != nil {
		result.OK, result.Error = false, err.Error()
	}
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, http.StatusOK, "model_test", result)
		return
	}
	page := newModelPage(r, p, m)
	page.Test = result
	s.render(w, http.StatusOK, "model.html", page)
}

// deleteModel removes a model from the project. The project's default
// model cannot go, nor a model an agent uses: another one must be
// chosen first.
func (s *server) deleteModel(w http.ResponseWriter, r *http.Request) {
	p, m, ok := s.openModel(w, r)
	if !ok {
		return
	}
	err := s.store.DeleteModel(r.Context(), p.ID, m.ID)
	if errors.Is(err, store.ErrModelIsDefault) {
		page := newSettingsPage(r, p)
		page.ModelError = "Not deleted: " + m.Name + " is the default model. Choose another default model first."
		s.renderSettings(w, r, http.StatusConflict, page)
		return
	}
	if inUse, ok := errors.AsType[*store.ModelInUseError](err); ok {
		page := newSettingsPage(r, p)
		if len(inUse.Agents) == 1 {
			page.ModelError = "Not deleted: the agent " + inUse.Agents[0] + " uses " + m.Name + ". Give it another model first."
		} else {
			page.ModelError = "Not deleted: the agents " + nameList(inUse.Agents) + " use " + m.Name + ". Give them another model first."
		}
		s.renderSettings(w, r, http.StatusConflict, page)
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=model-deleted#models", http.StatusSeeOther)
}

// setDefaultModel sets the project's default model, or clears it with
// the value 0. It needs project:edit.
func (s *server) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.openSettings(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	refuse := func() {
		page := newSettingsPage(r, p)
		page.ModelError = "Pick one of the project's models, or none."
		s.renderSettings(w, r, http.StatusBadRequest, page)
	}
	id, err := strconv.ParseInt(r.PostFormValue("model"), 10, 64)
	if err != nil || id < 0 {
		refuse()
		return
	}
	err = s.store.SetDefaultModel(r.Context(), p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		refuse()
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, projectPath(p.ID)+"/settings?saved=default#models", http.StatusSeeOther)
}

// modelPath is the URL of a model's page.
func modelPath(projectID, modelID int64) string {
	return projectPath(projectID) + "/models/" + strconv.FormatInt(modelID, 10)
}
