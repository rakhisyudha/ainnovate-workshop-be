package quiz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Domain errors returned by the repository. The service passes them upward
// and the handler maps them to HTTP status codes.
var (
	ErrLessonNotFound  = errors.New("lesson not found")
	ErrMasteryNotFound = errors.New("mastery not found")
	ErrQuizNotFound    = errors.New("quiz not found")
)

// Repository is the persistence contract used by the service.
//
// The service depends on this interface (not on Postgres directly), which is
// what makes the service unit-testable without a database. In production this
// interface might be satisfied by a real Postgres repository; in tests it is
// satisfied by a fake.
type Repository interface {
	GetLesson(ctx context.Context, id uuid.UUID) (Lesson, error)
	GetMastery(ctx context.Context, userID uuid.UUID, lessonID uuid.UUID) (Mastery, error)
	CreateQuiz(ctx context.Context, params CreateQuizParams) (Quiz, error)
	GetQuiz(ctx context.Context, id uuid.UUID) (Quiz, error)
}

// CreateQuizParams carries everything needed to persist a quiz atomically.
type CreateQuizParams struct {
	UserID     uuid.UUID
	LessonID   uuid.UUID
	Difficulty string
	Questions  []Question // CorrectOption and Position must be set.
}

// PostgresRepository talks to PostgreSQL using raw SQL through pgx.
//
// No ORM: every query is explicit. It also demonstrates two important pgx
// features used in the workshop:
//   - transactions (pgx.Tx)   -> all succeed together, or all fail together
//   - batches (pgx.Batch)     -> insert many rows with one round-trip
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewRepository wires a repository to a pgx connection pool.
func NewRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// ---------------------------------------------------------------------------
// TODO 1: Implement GetLesson
// ---------------------------------------------------------------------------
// Goal: load a single lesson by its id.
//
// Expected SQL:
//
//	SELECT id, title, objective, created_at
//	FROM lessons
//	WHERE id = $1
//
// Steps:
//  1. Use r.pool.QueryRow(ctx, query, id).
//  2. Scan the row into a Lesson value.
//  3. If the result is pgx.ErrNoRows, return ErrLessonNotFound.
//  4. Return the lesson.
//
// Always use $1 (parameterized query). Never build SQL with string
// concatenation — user input must never be pasted into SQL.
func (r *PostgresRepository) GetLesson(ctx context.Context, id uuid.UUID) (Lesson, error) {
	var lesson Lesson

	err := r.pool.QueryRow(ctx, `
		SELECT id, title, objective, created_at
		FROM lessons
		WHERE id = $1`, id,
	).Scan(&lesson.ID, &lesson.Title, &lesson.Objective, &lesson.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lesson{}, ErrLessonNotFound
	}
	if err != nil {
		return Lesson{}, fmt.Errorf("query lesson: %w", err)
	}

	return lesson, nil
}

// ---------------------------------------------------------------------------
// TODO 2: Implement GetMastery
// ---------------------------------------------------------------------------
// Goal: load the mastery score of a user for a lesson.
//
// Expected SQL:
//
//	SELECT user_id, lesson_id, score
//	FROM masteries
//	WHERE user_id = $1 AND lesson_id = $2
//
// Steps:
//  1. Use r.pool.QueryRow(ctx, query, userID, lessonID).
//  2. Scan the row into a Mastery value.
//  3. If the result is pgx.ErrNoRows, return ErrMasteryNotFound.
//  4. Return the mastery.
func (r *PostgresRepository) GetMastery(ctx context.Context, userID uuid.UUID, lessonID uuid.UUID) (Mastery, error) {
	var mastery Mastery

	err := r.pool.QueryRow(ctx, `
		SELECT user_id, lesson_id, score
		FROM masteries
		WHERE user_id = $1 AND lesson_id = $2`, userID, lessonID,
	).Scan(&mastery.UserID, &mastery.LessonID, &mastery.Score)
	if errors.Is(err, pgx.ErrNoRows) {
		return Mastery{}, ErrMasteryNotFound
	}
	if err != nil {
		return Mastery{}, fmt.Errorf("query mastery: %w", err)
	}

	return mastery, nil
}

// ---------------------------------------------------------------------------
// TODO 5 + 6: Implement CreateQuiz (transaction + pgx.Batch)
// ---------------------------------------------------------------------------
// Goal: persist a quiz and all of its questions ATOMICALLY.
//
// Workflow:
//
//	tx, err := r.pool.Begin(ctx)
//	defer tx.Rollback(ctx)              // no-op if already committed
//
//	INSERT INTO quizzes (id, user_id, lesson_id, difficulty)
//	VALUES ($1, $2, $3, $4)
//	RETURNING id, created_at
//
//	batch := &pgx.Batch{}
//	for i, question := range params.Questions {
//	    batch.Queue(
//	        `INSERT INTO questions
//	             (id, quiz_id, question, options, correct_option, position)
//	         VALUES ($1, $2, $3, $4, $5, $6)`,
//	        uuid.New(), quizID, question.Text, question.Options,
//	        question.CorrectOption, question.Position,
//	    )
//	}
//	results := tx.SendBatch(ctx, batch) // one round-trip for all questions
//	results.Close()
//
//	err = tx.Commit(ctx)                // everything persisted, or nothing
//
// How to pass options ([]string) to pgx so it lands in a JSONB column:
//
//	json.Marshal(question.Options)  -> []byte  (pgx stores []byte into jsonb)
//
// Expected return: a Quiz with ID and CreatedAt filled in.
//
// Rules to respect:
//   - if questions is empty, fail (an empty quiz must never be saved)
//   - use a transaction so quiz + questions commit or roll back together
//   - close the batch results (results.Close()) before committing
//   - always wrap low-level errors with %w
func (r *PostgresRepository) CreateQuiz(ctx context.Context, params CreateQuizParams) (Quiz, error) {
	if len(params.Questions) == 0 {
		return Quiz{}, errors.New("cannot create a quiz without questions")
	}

	// 1. Open a transaction.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // safe no-op if Commit already succeeded

	// 2. Insert the quiz.
	var created Quiz
	err = tx.QueryRow(ctx, `
		INSERT INTO quizzes (id, user_id, lesson_id, difficulty)
		VALUES ($1, $2, $3, $4)
		RETURNING id, user_id, lesson_id, difficulty, created_at`,
		uuid.New(), params.UserID, params.LessonID, params.Difficulty,
	).Scan(&created.ID, &created.UserID, &created.LessonID, &created.Difficulty, &created.CreatedAt)
	if err != nil {
		return Quiz{}, fmt.Errorf("insert quiz: %w", err)
	}

	// 3. Queue every question into one pgx.Batch (single round-trip).
	batch := &pgx.Batch{}

	for _, question := range params.Questions {
		optionsJSON, err := json.Marshal(question.Options)
		if err != nil {
			return Quiz{}, fmt.Errorf("encode question options: %w", err)
		}

		batch.Queue(`
			INSERT INTO questions (id, quiz_id, question, options, correct_option, position)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			question.ID, created.ID, question.Text, optionsJSON, question.CorrectOption, question.Position,
		)
	}

	// 4. Send the whole batch in one round-trip, then close it (required!).
	results := tx.SendBatch(ctx, batch)
	if err := results.Close(); err != nil {
		return Quiz{}, fmt.Errorf("insert questions: %w", err)
	}

	// 5. All-or-nothing: commit everything together.
	if err := tx.Commit(ctx); err != nil {
		return Quiz{}, fmt.Errorf("commit transaction: %w", err)
	}

	return created, nil
}

// ---------------------------------------------------------------------------
// GetQuiz (already implemented for you)
// ---------------------------------------------------------------------------
// Loads a quiz and its questions for the GET /v1/quizzes/{id} endpoint.
// Note that correct_option is loaded into memory but hidden from the API
// response by the model's `json:"-"` tag — the answer key never leaves the
// server.
func (r *PostgresRepository) GetQuiz(ctx context.Context, id uuid.UUID) (Quiz, error) {
	var q Quiz

	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, lesson_id, difficulty, created_at
		FROM quizzes
		WHERE id = $1`, id,
	).Scan(&q.ID, &q.UserID, &q.LessonID, &q.Difficulty, &q.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Quiz{}, ErrQuizNotFound
	}
	if err != nil {
		return Quiz{}, fmt.Errorf("query quiz: %w", err)
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, quiz_id, question, options, correct_option, position, created_at
		FROM questions
		WHERE quiz_id = $1
		ORDER BY position`, id)
	if err != nil {
		return Quiz{}, fmt.Errorf("query questions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var question Question
		var optionsJSON []byte

		if err := rows.Scan(
			&question.ID,
			&question.QuizID,
			&question.Text,
			&optionsJSON,
			&question.CorrectOption,
			&question.Position,
			&question.CreatedAt,
		); err != nil {
			return Quiz{}, fmt.Errorf("scan question: %w", err)
		}

		if err := json.Unmarshal(optionsJSON, &question.Options); err != nil {
			return Quiz{}, fmt.Errorf("decode question options: %w", err)
		}

		q.Questions = append(q.Questions, question)
	}

	if err := rows.Err(); err != nil {
		return Quiz{}, fmt.Errorf("iterate questions: %w", err)
	}

	return q, nil
}
