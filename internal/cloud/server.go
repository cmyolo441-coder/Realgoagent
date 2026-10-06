package cloud

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
	"github.com/nova-ai/nova/internal/subagent"
	"github.com/nova-ai/nova/internal/tools"
)

// Server runs cloud sessions: each session is an agent with its own
// workspace directory, and terminals stream its events over SSE.
//
// The tools package keeps its workspace in process-global state, so turns
// are serialized server-wide: one session runs at a time. Sessions stay
// independent and persistent — a turn on session B simply waits for the
// turn on session A to finish.
type Server struct {
	dir string
	cfg *config.Config

	mu       sync.Mutex
	sessions map[string]*Session

	// turnMu serializes turns across sessions (see above).
	turnMu sync.Mutex

	token string
}

// Session is one cloud agent session.
type Session struct {
	info      SessionInfo
	dir       string // session dir
	workspace string // git workspace inside dir

	mu      sync.Mutex
	ag      *agent.Agent
	running bool

	subMu sync.Mutex
	subs  map[chan agent.Event]struct{}

	// answerCh carries ask_user answers from the terminal to the agent.
	answerCh chan string
	// questionMu guards questionPending.
	questionMu      sync.Mutex
	questionPending bool
	// done closes when the session is deleted.
	done chan struct{}
}

// NewServer creates a server rooted at dir (sessions, workspaces).
func NewServer(dir string, cfg *config.Config, token string) *Server {
	return &Server{
		dir:      dir,
		cfg:      cfg,
		sessions: make(map[string]*Session),
		token:    token,
	}
}

// newID mints a short session id.
func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())[6:14]
}

// Create starts a session, cloning repo into its workspace when given.
func (s *Server) Create(req CreateRequest) (*SessionInfo, error) {
	id := newID()
	sdir := filepath.Join(s.dir, "sessions", id)
	ws := filepath.Join(sdir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}

	if req.Repo != "" {
		if err := gitClone(req.Repo, ws); err != nil {
			os.RemoveAll(sdir)
			return nil, fmt.Errorf("clone %s: %w", req.Repo, err)
		}
		if req.Branch != "" {
			if err := gitCheckout(ws, req.Branch); err != nil {
				os.RemoveAll(sdir)
				return nil, fmt.Errorf("checkout %s: %w", req.Branch, err)
			}
		}
	}

	name := req.Name
	if name == "" {
		name = "session-" + id
	}
	sess := &Session{
		info: SessionInfo{
			ID:        id,
			Name:      name,
			Repo:      req.Repo,
			Branch:    req.Branch,
			Status:    "idle",
			CreatedAt: time.Now(),
		},
		dir:       sdir,
		workspace: ws,
		subs:      make(map[chan agent.Event]struct{}),
		answerCh:  make(chan string, 1),
		done:      make(chan struct{}),
	}
	if err := s.buildAgent(sess); err != nil {
		os.RemoveAll(sdir)
		return nil, err
	}

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	info := sess.info
	return &info, nil
}

// buildAgent wires an agent for the session. The event sink broadcasts to
// SSE subscribers; ask_user blocks until the terminal answers.
func (s *Server) buildAgent(sess *Session) error {
	cfg := s.cfg
	p, m, err := cfg.ResolveModel(cfg.DefaultModel)
	if err != nil {
		return fmt.Errorf("resolve model: %w", err)
	}
	reg := tools.NewRegistry()
	tools.RegisterDefaults(reg)

	sessRef := sess
	ag, err := agent.New(agent.Options{
		Config:    cfg,
		Provider:  p,
		Model:     m,
		Workspace: sess.workspace,
		Registry:  reg,
		EventSink: func(ev agent.Event) {
			sessRef.broadcast(ev)
		},
		OnAskUser: func(q string) string {
			// Mark the question pending before broadcasting, so an answer
			// that arrives fast is accepted rather than refused.
			sessRef.questionMu.Lock()
			sessRef.questionPending = true
			// Drain any stale answer from a previous round.
			select {
			case <-sessRef.answerCh:
			default:
			}
			sessRef.questionMu.Unlock()
			sessRef.broadcast(agent.Event{Kind: agent.EvAskUser, Text: q})
			select {
			case ans := <-sessRef.answerCh:
				sessRef.questionMu.Lock()
				sessRef.questionPending = false
				sessRef.questionMu.Unlock()
				return ans
			case <-sessRef.done:
				return "(session closed before the question was answered)"
			}
		},
	})
	if err != nil {
		return err
	}
	sess.ag = ag
	return nil
}

// broadcast sends an event to every SSE subscriber (non-blocking).
func (sess *Session) broadcast(ev agent.Event) {
	sess.subMu.Lock()
	defer sess.subMu.Unlock()
	for ch := range sess.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe registers an SSE subscriber; the returned func unsubscribes.
func (sess *Session) subscribe() (<-chan agent.Event, func()) {
	ch := make(chan agent.Event, 256)
	sess.subMu.Lock()
	sess.subs[ch] = struct{}{}
	sess.subMu.Unlock()
	return ch, func() {
		sess.subMu.Lock()
		delete(sess.subs, ch)
		sess.subMu.Unlock()
	}
}

// Prompt starts a turn. It returns once the turn is queued; events stream
// over SSE.
func (s *Server) Prompt(id, text string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.mu.Lock()
	if sess.running {
		sess.mu.Unlock()
		return fmt.Errorf("a turn is already running on this session")
	}
	sess.running = true
	sess.info.Status = "running"
	sess.mu.Unlock()

	go func() {
		// Serialize turns server-wide: the tools' workspace, edit watcher
		// and subagent runner are process-global.
		s.turnMu.Lock()
		defer s.turnMu.Unlock()
		defer func() {
			sess.mu.Lock()
			sess.running = false
			sess.info.Status = "idle"
			sess.mu.Unlock()
		}()

		tools.SetWorkspace(sess.workspace, s.cfg.Shell)
		tools.SetEditWatcher(func(rec tools.EditRecord) {
			sess.broadcast(agent.Event{Kind: agent.EvEdit, Tool: rec.Tool, Edit: &rec})
		})
		subagent.Install(&subagent.Runner{
			Config:    s.cfg,
			Workspace: sess.workspace,
			OnEvent: func(ev subagent.Event) {
				// Subagent progress surfaces as plain text on the session.
				sess.broadcast(agent.Event{Kind: agent.EvText, Text: ev.Text})
			},
		})

		_ = sess.ag.Run(text)
	}()
	return nil
}

// Cancel stops the session's running turn.
func (s *Server) Cancel(id string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.ag.Cancel()
	return nil
}

// Answer delivers an ask_user answer to the session's agent.
func (s *Server) Answer(id, text string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.questionMu.Lock()
	pending := sess.questionPending
	sess.questionMu.Unlock()
	if !pending {
		return fmt.Errorf("no question is pending on this session")
	}
	select {
	case sess.answerCh <- text:
		return nil
	default:
		return fmt.Errorf("an answer was already given")
	}
}

// Info returns a session's info snapshot.
func (s *Server) Info(id string) (*SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such session: %s", id)
	}
	info := sess.info
	return &info, nil
}

// List returns all sessions.
func (s *Server) List() []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess.info)
	}
	return out
}

// Delete removes a session.
func (s *Server) Delete(id string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	close(sess.done)
	sess.ag.Cancel()
	os.RemoveAll(sess.dir)
	return nil
}

// Reset clears the session's conversation.
func (s *Server) Reset(id string) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.running {
		return fmt.Errorf("cannot reset while a turn is running")
	}
	sess.ag.Reset()
	return nil
}

// SetPlanMode toggles plan mode on the session's agent.
func (s *Server) SetPlanMode(id string, on bool) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.ag.SetPlanMode(on)
	return nil
}
func (s *Server) InjectHistory(id string, msgs []llm.Message) error {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such session: %s", id)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.running {
		return fmt.Errorf("cannot inject history while a turn is running")
	}
	sess.ag.Reset()
	sess.ag.InjectHistory(msgs)
	return nil
}

func gitClone(repo, dir string) error {
	// Clone into the (empty) workspace dir via a temp parent: git refuses
	// to clone into a non-empty directory, and MkdirAll created it.
	parent := filepath.Dir(dir)
	tmp, err := os.MkdirTemp(parent, "clone-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.Command("git", "clone", "--depth", "50", repo, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

func gitCheckout(dir, branch string) error {
	cmd := exec.Command("git", "-C", dir, "checkout", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

