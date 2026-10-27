package tomb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gluck/ebac/internal/forge"
)

type TeamsMsg struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	ComposeTime string         `json:"composetime"`
	Arrival     string         `json:"originalarrivaltime"`
	MessageType string         `json:"messagetype"`
	From        string         `json:"imdisplayname"`
	FromURL     string         `json:"from"`
	TokenName   string         `json:"fromDisplayNameInToken"`
	Content     string         `json:"content"`
	Properties  map[string]any `json:"properties"`
}

func (m TeamsMsg) IsMessage() bool {
	return m.MessageType == "RichText/Html" || m.MessageType == "Text" || strings.HasPrefix(m.MessageType, "RichText")
}

func (m TeamsMsg) Deleted() bool {
	if m.Properties != nil {
		if v, ok := m.Properties["deletetime"]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" {
			return true
		}
	}
	return strings.TrimSpace(m.Content) == ""
}

// senderID is the last segment of the from URL: 8:orgid:<guid> for a person.
func (m TeamsMsg) SenderID() string {
	return m.FromURL[strings.LastIndex(m.FromURL, "/")+1:]
}

// sender names who wrote m: the row's name, the token's, a name learned from
// another row of the same sender, and at worst the sender id itself.
func (m TeamsMsg) Sender(names map[string]string) string {
	for _, n := range []string{m.From, m.TokenName, names[m.SenderID()], m.SenderID()} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return "unknown sender"
}

func (m TeamsMsg) HasImage() bool { return strings.Contains(m.Content, "<img") }

// Tomb runs the tomb CLI. Its cwd is a scratch dir: tomb writes teams.log
// into the cwd unless told otherwise, and --log ” is the otherwise.
type Tomb struct {
	Bin     string
	Timeout time.Duration
	// Cred mints the Teams token (band-cred teams-ebac), handed to tomb on fd 3
	// with TOMB_AAD_TOKEN_FD=3: the env carries the fd number, never the token.
	// nil only in tests that fake tomb.
	Cred *forge.Cred
}

var ErrTombGone = errors.New("chat gone")

// Only the CHAT API's 404 or 403 means the chat may be gone. tomb's auth
// errors read "auth service returned 403: ..." and an expired login must stay
// transient: matching any "returned 403" would stop the critic for good after
// three rounds of a sign-in problem.
var teamsGoneText = regexp.MustCompile(`\bchat API returned (404|403)\b`)

func (t *Tomb) Messages(ctx context.Context, conv string, limit int) ([]TeamsMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	cmd := forge.ExecCommandContext(ctx, t.Bin, "--log", "", "--json", "--raw", "messages", conv, "--limit", fmt.Sprint(limit))
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	if t.Cred != nil {
		tok, err := t.Cred.Token(ctx)
		if err != nil {
			return nil, err
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		// A token is a few KB, far under the pipe buffer: write it all, then close.
		if _, err := w.WriteString(tok); err != nil {
			w.Close()
			return nil, err
		}
		w.Close()
		cmd.ExtraFiles = []*os.File{r} // fd 3 in the child
		cmd.Env = append(cmd.Env, "TOMB_AAD_TOKEN_FD=3")
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("tomb timed out after %s", t.Timeout)
		}
		if teamsGoneText.MatchString(msg) {
			return nil, fmt.Errorf("%w: %s", ErrTombGone, msg)
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tomb: %s", msg)
	}
	var resp struct {
		Messages []TeamsMsg `json:"messages"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("tomb: unreadable answer: %w", err)
	}
	return resp.Messages, nil
}

const TeamsTombWait = 60 * time.Second
