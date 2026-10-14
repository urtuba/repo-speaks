package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The roles of a message.
const (
	RoleUser      = "user"      // a question
	RoleAssistant = "assistant" // an answer
)

// The statuses of an answer.
const (
	AnswerRunning = "running"
	AnswerDone    = "done"
	AnswerFailed  = "failed"
)

// maxTitle is the longest chat title, in characters. A longer first
// question is cut.
const maxTitle = 80

// ErrAnswerRunning is returned when a question comes in while the chat
// still waits for an answer.
var ErrAnswerRunning = errors.New("an answer is still running in the chat")

// Chat is a conversation between one member and one agent.
type Chat struct {
	ID        int64
	ProjectID int64
	// AgentID is 0 when the agent was deleted. AgentName and AgentKind
	// are then empty.
	AgentID   int64
	AgentName string
	AgentKind string
	// UserID is 0 when the account was deleted. Username is then empty.
	UserID    int64
	Username  string
	Title     string
	CreatedAt time.Time
}

// Message is a question or an answer. The fields after Content are
// used by answers only.
type Message struct {
	ID      int64
	ChatID  int64
	Role    string // RoleUser or RoleAssistant
	Content string
	Status  string // AnswerRunning, AnswerDone or AnswerFailed
	Error   string // why the answer failed
	// Progress is what the agent did so far, one entry per step.
	Progress   []string
	Usage      AnswerUsage
	CreatedAt  time.Time
	FinishedAt time.Time // zero while running
	Sources    []Source
}

// AnswerUsage is what one answer used.
type AnswerUsage struct {
	ModelID          string // the host's ID of the model that answered
	PromptTokens     int
	CompletionTokens int
	Steps            int // tool calls made
}

// Source is a file an answer cites. Owner and Repo come from the
// resource, so a source of a deleted resource is gone with it.
type Source struct {
	Position   int
	ResourceID int64
	Owner      string
	Repo       string
	Path       string // the file in the resource, such as app/billing.go
	LineFrom   int
	LineTo     int
	CommitID   string // the clone's commit when the question started
}

// NewSource is a source to save with an answer.
type NewSource struct {
	ResourceID int64
	Path       string
	LineFrom   int
	LineTo     int
	CommitID   string
}

// chatTitle makes a title from the first question: one line, cut to
// maxTitle characters.
func chatTitle(question string) string {
	title := strings.Join(strings.FieldsFunc(question, unicode.IsSpace), " ")
	if utf8.RuneCountInString(title) > maxTitle {
		title = strings.TrimSpace(string([]rune(title)[:maxTitle])) + "..."
	}
	return title
}

// StartChat starts a chat with the agent: the chat, the first question
// and a running answer, in one transaction. It returns the chat's ID and
// the answer's ID. It returns ErrNotFound when the project has no such
// agent. The caller has checked the question and the agent's kind.
func (s *Store) StartChat(ctx context.Context, projectID, agentID, userID int64, question string, now time.Time) (chatID, answerID int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("start chat: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	err = tx.QueryRowContext(ctx, `
		INSERT INTO chats (project_id, agent_id, user_id, title, created_at)
		SELECT project_id, id, ?, ?, ? FROM agents WHERE id = ? AND project_id = ?
		RETURNING id`,
		userID, chatTitle(question), now.Unix(), agentID, projectID).Scan(&chatID)
	if err != nil {
		return 0, 0, fmt.Errorf("start chat with agent %d: %w", agentID, noRows(err))
	}
	if answerID, err = addQuestion(ctx, tx, chatID, question, now); err != nil {
		return 0, 0, fmt.Errorf("start chat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("start chat: %w", err)
	}
	return chatID, answerID, nil
}

// AddQuestion adds a question and a running answer to the project's
// chat, in one transaction. It returns the answer's ID. It returns
// ErrNotFound when the project has no such chat, and ErrAnswerRunning
// while an earlier answer still runs.
func (s *Store) AddQuestion(ctx context.Context, projectID, chatID int64, question string, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add question: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	var exists, running bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM chats WHERE id = ?1 AND project_id = ?2),
			EXISTS (SELECT 1 FROM messages WHERE chat_id = ?1 AND status = 'running')`,
		chatID, projectID).Scan(&exists, &running); err != nil {
		return 0, fmt.Errorf("add question: %w", err)
	}
	if !exists {
		return 0, fmt.Errorf("chat %d: %w", chatID, ErrNotFound)
	}
	if running {
		return 0, ErrAnswerRunning
	}
	answerID, err := addQuestion(ctx, tx, chatID, question, now)
	if err != nil {
		return 0, fmt.Errorf("add question: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("add question: %w", err)
	}
	return answerID, nil
}

// addQuestion adds the question and a running answer to the chat, and
// returns the answer's ID.
func addQuestion(ctx context.Context, tx *sql.Tx, chatID int64, question string, now time.Time) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO messages (chat_id, role, content, created_at) VALUES (?, 'user', ?, ?)",
		chatID, question, now.Unix()); err != nil {
		return 0, err
	}
	var answerID int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO messages (chat_id, role, status, created_at) VALUES (?, 'assistant', 'running', ?)
		RETURNING id`, chatID, now.Unix()).Scan(&answerID)
	return answerID, err
}

// chatColumns is the SELECT list scanChat reads. The agent and the
// account come from LEFT JOINs, since either may be deleted.
const chatColumns = `
	c.id, c.project_id, c.agent_id, a.name, a.kind, c.user_id, u.username, c.title, c.created_at
	FROM chats c
	LEFT JOIN agents a ON a.id = c.agent_id
	LEFT JOIN users u ON u.id = c.user_id`

// scanChat reads one row selected with chatColumns.
func scanChat(row scanner) (Chat, error) {
	var c Chat
	var agentID, userID sql.NullInt64
	var agentName, agentKind, username sql.NullString
	var createdAt int64
	err := row.Scan(&c.ID, &c.ProjectID, &agentID, &agentName, &agentKind, &userID, &username,
		&c.Title, &createdAt)
	if err != nil {
		return Chat{}, noRows(err)
	}
	c.AgentID = agentID.Int64
	c.AgentName = agentName.String
	c.AgentKind = agentKind.String
	c.UserID = userID.Int64
	c.Username = username.String
	c.CreatedAt = unix(createdAt)
	return c, nil
}

// Chat returns the project's chat with the id. It returns ErrNotFound
// when the project has no such chat, so one project's URL cannot reach
// another project's chat.
func (s *Store) Chat(ctx context.Context, projectID, id int64) (Chat, error) {
	row := s.db.QueryRowContext(ctx, "SELECT"+chatColumns+`
		WHERE c.id = ? AND c.project_id = ?`, id, projectID)
	c, err := scanChat(row)
	if err != nil {
		return Chat{}, fmt.Errorf("chat %d: %w", id, err)
	}
	return c, nil
}

// UserChats returns the chats the account started with the agent in the
// project, newest first.
func (s *Store) UserChats(ctx context.Context, projectID, userID, agentID int64) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+chatColumns+`
		WHERE c.project_id = ? AND c.user_id = ? AND c.agent_id = ?
		ORDER BY c.id DESC`, projectID, userID, agentID)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	return scanChats(rows)
}

// ProjectChats returns the project's newest chats, at most limit, newest
// first.
func (s *Store) ProjectChats(ctx context.Context, projectID int64, limit int) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+chatColumns+`
		WHERE c.project_id = ?
		ORDER BY c.id DESC
		LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	return scanChats(rows)
}

// scanChats reads every row selected with chatColumns, and closes rows.
func scanChats(rows *sql.Rows) ([]Chat, error) {
	defer rows.Close()
	var list []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, fmt.Errorf("list chats: %w", err)
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// Messages returns the messages of the project's chat, oldest first,
// each answer with its sources. A chat of another project has none.
func (s *Store) Messages(ctx context.Context, projectID, chatID int64) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.chat_id, m.role, m.content, m.status, m.error, m.progress, m.model_id,
			m.prompt_tokens, m.completion_tokens, m.steps, m.created_at, m.finished_at
		FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE m.chat_id = ? AND c.project_id = ?
		ORDER BY m.id`, chatID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var list []Message
	byID := map[int64]int{} // message ID to its index in list
	for rows.Next() {
		var m Message
		var status, errText, modelID sql.NullString
		var progress string
		var createdAt int64
		var finishedAt sql.NullInt64
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &status, &errText, &progress, &modelID,
			&m.Usage.PromptTokens, &m.Usage.CompletionTokens, &m.Usage.Steps, &createdAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		m.Status = status.String
		m.Error = errText.String
		m.Usage.ModelID = modelID.String
		if progress != "" {
			m.Progress = strings.Split(strings.TrimSuffix(progress, "\n"), "\n")
		}
		m.CreatedAt = unix(createdAt)
		if finishedAt.Valid {
			m.FinishedAt = unix(finishedAt.Int64)
		}
		byID[m.ID] = len(list)
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	if err := s.addSources(ctx, projectID, chatID, list, byID); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return list, nil
}

// addSources reads the sources of the chat's answers into list.
func (s *Store) addSources(ctx context.Context, projectID, chatID int64, list []Message, byID map[int64]int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ms.message_id, ms.position, ms.resource_id, r.owner, r.repo, ms.path,
			ms.line_from, ms.line_to, ms.commit_id
		FROM message_sources ms
		JOIN messages m ON m.id = ms.message_id
		JOIN resources r ON r.id = ms.resource_id AND r.project_id = ?
		WHERE m.chat_id = ?
		ORDER BY ms.message_id, ms.position`, projectID, chatID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID int64
		var src Source
		if err := rows.Scan(&messageID, &src.Position, &src.ResourceID, &src.Owner, &src.Repo, &src.Path,
			&src.LineFrom, &src.LineTo, &src.CommitID); err != nil {
			return err
		}
		if i, ok := byID[messageID]; ok {
			list[i].Sources = append(list[i].Sources, src)
		}
	}
	return rows.Err()
}

// inProjectAnswer limits an UPDATE of messages to a running answer in a
// chat of the project. Its parameters are the message's ID and the
// project's ID.
const inProjectAnswer = `
	WHERE id = ? AND status = 'running'
	AND chat_id IN (SELECT id FROM chats WHERE project_id = ?)`

// AddAnswerStep adds one line to a running answer's progress. It
// returns ErrNotFound when the project has no such running answer.
func (s *Store) AddAnswerStep(ctx context.Context, projectID, answerID int64, step string) error {
	// A step is one line, so a line break inside it would split it.
	step = strings.ReplaceAll(step, "\n", " ")
	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET progress = progress || ? || char(10)`+inProjectAnswer,
		step, answerID, projectID)
	if err != nil {
		return fmt.Errorf("add answer step: %w", err)
	}
	return oneRow(res, fmt.Sprintf("running answer %d", answerID))
}

// FinishAnswer saves a running answer's text, usage and sources, and
// marks it done, in one transaction. Only resources of the project are
// saved as sources. It returns ErrNotFound when the project has no such
// running answer.
func (s *Store) FinishAnswer(ctx context.Context, projectID, answerID int64, content string, sources []NewSource, u AnswerUsage, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish answer: %w", err)
	}
	defer tx.Rollback() // a no-op after Commit

	res, err := tx.ExecContext(ctx, `
		UPDATE messages SET status = 'done', content = ?, model_id = ?, prompt_tokens = ?,
			completion_tokens = ?, steps = ?, finished_at = ?`+inProjectAnswer,
		content, nullText(u.ModelID), u.PromptTokens, u.CompletionTokens, u.Steps, now.Unix(),
		answerID, projectID)
	if err != nil {
		return fmt.Errorf("finish answer: %w", err)
	}
	if err := oneRow(res, fmt.Sprintf("running answer %d", answerID)); err != nil {
		return err
	}
	position := 0
	for _, src := range sources {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO message_sources (message_id, position, resource_id, path, line_from, line_to, commit_id)
			SELECT ?, ?, id, ?, ?, ?, ? FROM resources WHERE id = ? AND project_id = ?`,
			answerID, position+1, src.Path, src.LineFrom, src.LineTo, src.CommitID, src.ResourceID, projectID)
		if err != nil {
			return fmt.Errorf("finish answer: %w", err)
		}
		// A resource deleted during the answer has no row to cite.
		if n, err := res.RowsAffected(); err == nil && n == 1 {
			position++
		}
	}
	return tx.Commit()
}

// FailAnswer marks a running answer failed, with the reason and what it
// used so far. msg is shown to the asker, so it must be safe to show.
// It returns ErrNotFound when the project has no such running answer.
func (s *Store) FailAnswer(ctx context.Context, projectID, answerID int64, msg string, u AnswerUsage, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET status = 'failed', error = ?, model_id = ?, prompt_tokens = ?,
			completion_tokens = ?, steps = ?, finished_at = ?`+inProjectAnswer,
		msg, nullText(u.ModelID), u.PromptTokens, u.CompletionTokens, u.Steps, now.Unix(),
		answerID, projectID)
	if err != nil {
		return fmt.Errorf("fail answer: %w", err)
	}
	return oneRow(res, fmt.Sprintf("running answer %d", answerID))
}

// FailRunningAnswers marks every running answer failed, with msg. The
// server calls it on start: answers run inside the server process, so
// one still running then was cut off by a stop or a crash. It returns
// how many answers it marked.
func (s *Store) FailRunningAnswers(ctx context.Context, msg string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET status = 'failed', error = ?, finished_at = ?
		WHERE status = 'running'`, msg, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("fail running answers: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fail running answers: %w", err)
	}
	return n, nil
}
