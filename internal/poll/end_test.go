package poll

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gluck/ebac/internal/model"
)

// An ended critic's kills: children first in key order, the role form last, and only when every child is gone; a
// child that fails stops the role kill and names itself (retried at the next listing). RED if the role form goes
// before its children, or a failure is swallowed.
func TestEndKillsChildrenThenTheRoleForm(t *testing.T) {
	ended := time.Now()
	w := &model.Critic{Name: "keel-x", FormID: "@role"}
	w.Runtime.Archived, w.Runtime.EndedAt = true, &ended
	w.Runtime.Children = map[string]string{"b": "@child-b", "a": "@child-a"}
	var killed []string
	fail := ""
	var out bytes.Buffer
	p := &Poller{Out: &out, Kill: func(_ context.Context, id string) error {
		if id == fail {
			return errors.New("broker refused")
		}
		killed = append(killed, id)
		return nil
	}}
	if err := p.End(context.Background(), w, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(killed, ",") != "@child-a,@child-b,@role" {
		t.Fatalf("kill order %v", killed)
	}
	killed, fail = nil, "@child-b"
	err := p.End(context.Background(), w, "")
	if err == nil || !strings.Contains(err.Error(), "1 child form(s) not removed") || strings.Contains(strings.Join(killed, ","), "@role") {
		t.Fatalf("a failed child: err %v killed %v (the role form must stay)", err, killed)
	}
}
