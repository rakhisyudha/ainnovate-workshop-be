package quiz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"arahin-mini/internal/ai"

	"github.com/google/uuid"
)

// RequestStatus models the lifecycle of an asynchronous quiz request.
type RequestStatus string

const (
	StatusProcessing RequestStatus = "processing"
	StatusReady      RequestStatus = "ready"
	StatusFailed     RequestStatus = "failed"
)

// QuizRequest is a quiz-generation job tracked in memory.
//
// Workshop note: production Arah.in uses a durable queue (Google Cloud Tasks)
// so jobs survive restarts and retries. Here we keep state in memory on
// purpose to keep the focus on the workflow, not the infrastructure.
type QuizRequest struct {
	ID        uuid.UUID     `json:"request_id"`
	UserID    uuid.UUID     `json:"-"`
	LessonID  uuid.UUID     `json:"-"`
	Status    RequestStatus `json:"status"`
	QuizID    *uuid.UUID    `json:"quiz_id,omitempty"`
	Message   string        `json:"message,omitempty"`
	CreatedAt time.Time     `json:"-"`
}

// ErrRequestNotFound is returned when GET /v1/quiz-requests/{id} sees an
// unknown request id.
var ErrRequestNotFound = errors.New("quiz request not found")

// requestStore is a tiny in-memory job registry guarded by a mutex.
// It is intentionally simple: map + sync.Mutex.
type requestStore struct {
	mu   sync.Mutex
	jobs map[uuid.UUID]*QuizRequest
}

func newRequestStore() *requestStore {
	return &requestStore{jobs: make(map[uuid.UUID]*QuizRequest)}
}

func (s *requestStore) put(req *QuizRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[req.ID] = req
}

// get returns a copy of a job so callers never race with the worker goroutine.
func (s *requestStore) get(id uuid.UUID) (*QuizRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	req, ok := s.jobs[id]
	if !ok {
		return nil, false
	}

	copy := *req
	return &copy, true
}

func (s *requestStore) markReady(id uuid.UUID, quizID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.jobs[id]; ok {
		req.Status = StatusReady
		req.QuizID = &quizID
	}
}

func (s *requestStore) markFailed(id uuid.UUID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.jobs[id]; ok {
		req.Status = StatusFailed
		req.Message = message
	}
}

// Service contains the business logic of quiz generation. It sits between the
// handler (HTTP) and the repository (PostgreSQL) plus the AI client.
type Service struct {
	repo  Repository
	ai    ai.AIClient
	store *requestStore
}

// NewService wires the service with its dependencies (manual DI).
func NewService(repo Repository, client ai.AIClient) *Service {
	return &Service{
		repo:  repo,
		ai:    client,
		store: newRequestStore(),
	}
}

// StartGeneration accepts a quiz-generation request and returns immediately
// (HTTP 202). The heavy work runs in a background goroutine so the API never
// blocks on the AI call.
func (s *Service) StartGeneration(ctx context.Context, userID, lessonID uuid.UUID) (*QuizRequest, error) {
	req := &QuizRequest{
		ID:        uuid.New(),
		UserID:    userID,
		LessonID:  lessonID,
		Status:    StatusProcessing,
		CreatedAt: time.Now(),
	}

	s.store.put(req)

	slog.Info("request received",
		"request_id", req.ID,
		"user_id", userID,
		"lesson_id", lessonID,
	)

	// Snapshot BEFORE the worker starts, so the reader never races the
	// goroutine that will mutate the shared job object.
	snapshot := *req

	// Background job. NOTE: we use context.Background(), not the HTTP request
	// context — the request is already over and no one is waiting anymore.
	go s.generateQuiz(req)

	return &snapshot, nil
}

// generateQuiz runs the whole pipeline and updates the job status.
// The full workflow:
//
//	load lesson -> load mastery -> build AI request -> call AI ->
//	validate AI response -> transaction (create quiz + batch questions) -> ready
func (s *Service) generateQuiz(req *QuizRequest) {
	ctx := context.Background()

	slog.Info("generation started", "request_id", req.ID)

	quiz, err := s.runGeneration(ctx, req)
	if err != nil {
		slog.Error("generation failed", "request_id", req.ID, "error", err)
		s.store.markFailed(req.ID, err.Error())
		return
	}

	s.store.markReady(req.ID, quiz.ID)

	slog.Info("quiz created",
		"request_id", req.ID,
		"quiz_id", quiz.ID,
		"difficulty", quiz.Difficulty,
	)
}

// runGeneration executes the ordered steps of the pipeline.
func (s *Service) runGeneration(ctx context.Context, req *QuizRequest) (Quiz, error) {
	// 1. Load the lesson so the AI knows what to teach.
	lesson, err := s.repo.GetLesson(ctx, req.LessonID)
	if err != nil {
		return Quiz{}, fmt.Errorf("load lesson: %w", err)
	}

	// 2. Load mastery so the AI can adapt to the learner.
	mastery, err := s.repo.GetMastery(ctx, req.UserID, req.LessonID)
	if err != nil {
		return Quiz{}, fmt.Errorf("load mastery: %w", err)
	}

	// 3. Build the AI context from lesson + mastery.
	aiRequest, err := s.buildAIRequest(ctx, lesson, mastery)
	if err != nil {
		return Quiz{}, fmt.Errorf("build AI request: %w", err)
	}

	// 4. Call the AI provider (stub in the workshop, real provider later).
	aiResponse, err := s.ai.GenerateQuiz(ctx, aiRequest)
	if err != nil {
		slog.Error("AI generation failed", "request_id", req.ID, "error", err)
		return Quiz{}, fmt.Errorf("AI generation: %w", err)
	}

	// 5. TODO 4: validate the AI response BEFORE saving.
	// The validation function ai.ValidateResponse already exists and is unit
	// tested in internal/ai/client.go. Wire it in now:
	//
	if err := ai.ValidateResponse(aiResponse); err != nil {
		slog.Error("validation failed",
			"request_id", req.ID,
			"error", err,
		)
		return Quiz{}, fmt.Errorf("validate AI response: %w", err)
	}
	//
	// If validation is skipped, bad AI output would be trusted and saved.
	// "AI generates. Backend governs." — never save an unvalidated response.

	// 6. Persist quiz + questions atomically.
	created, err := s.repo.CreateQuiz(ctx, CreateQuizParams{
		UserID:     req.UserID,
		LessonID:   req.LessonID,
		Difficulty: aiResponse.Difficulty,
		Questions:  toQuestions(aiResponse),
	})
	if err != nil {
		return Quiz{}, fmt.Errorf("save quiz: %w", err)
	}

	return created, nil
}

// toQuestions maps AI questions into persisted Question rows.
func toQuestions(resp ai.GenerateQuizResponse) []Question {
	questions := make([]Question, 0, len(resp.Questions))
	for i, q := range resp.Questions {
		questions = append(questions, Question{
			ID:            uuid.New(),
			Text:          q.Question,
			Options:       q.Options,
			CorrectOption: q.CorrectOption,
			Position:      i,
		})
	}
	return questions
}

// ---------------------------------------------------------------------------
// TODO 3: Implement buildAIRequest
// ---------------------------------------------------------------------------
// Goal: translate the server domain knowledge (lesson + mastery) into the
// AI provider's request shape.
//
// The AI needs to understand:
//
//	objective  -> what the quiz must test
//	mastery    -> how advanced the learner is, so difficulty can be tuned
//
// Expected implementation:
//
//	return ai.GenerateQuizRequest{
//	    Objective: lesson.Objective,
//	    Mastery:   mastery.Score,
//	}, nil
//
// Bonus (optional): if mastery is high, tell the AI to ask harder questions.
// The stub already derives difficulty from mastery, so the base version above
// works for the whole workshop.
func (s *Service) buildAIRequest(ctx context.Context, lesson Lesson, mastery Mastery) (ai.GenerateQuizRequest, error) {
	return ai.GenerateQuizRequest{
		Objective: lesson.Objective,
		Mastery:   mastery.Score,
	}, nil
}

// GetQuizRequest returns the current status of a generation job.
func (s *Service) GetQuizRequest(ctx context.Context, requestID uuid.UUID) (*QuizRequest, error) {
	req, ok := s.store.get(requestID)
	if !ok {
		return nil, ErrRequestNotFound
	}
	return req, nil
}

// GetQuiz returns a generated quiz. The answer key is hidden at the model
// layer (json:"-"), so it never reaches the API response.
func (s *Service) GetQuiz(ctx context.Context, quizID uuid.UUID) (Quiz, error) {
	return s.repo.GetQuiz(ctx, quizID)
}
