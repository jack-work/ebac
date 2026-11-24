// praxis-check validates and renders one praxis issue form for tools outside the gh-cache sync (the
// hourly praxis lint, the end-to-end proof). stdin: {"form": {...praxis.* keys...}, "parent": {"number": n,
// "open": true, "status": "..."} | null}. `praxis-check validate` exits 0 when clean, 3 with
// {"rule","detail","alarm"} on stdout when refused, 2 on bad input. `praxis-check render` prints
// {"title","body","claim"} (exit 3 on a refusal, as validate).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gluck/ebac/internal/praxis"
)

type input struct {
	Form   map[string]any `json:"form"`
	Parent *struct {
		Number int           `json:"number"`
		Open   bool          `json:"open"`
		Status praxis.Status `json:"status"`
	} `json:"parent"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

// run is main without the process: it returns the exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	emit := func(code int, v any) int {
		_ = json.NewEncoder(stdout).Encode(v)
		return code
	}
	if len(args) != 1 || (args[0] != "validate" && args[0] != "render") {
		fmt.Fprintln(stderr, "usage: praxis-check validate|render < input.json")
		return 2
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return emit(2, map[string]string{"error": err.Error()})
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return emit(2, map[string]string{"error": "input: " + err.Error()})
	}
	issue, err := praxis.FromForm(in.Form)
	if err != nil {
		return emit(2, map[string]string{"error": "form: " + err.Error()})
	}
	var parent *praxis.ParentState
	if in.Parent != nil {
		parent = &praxis.ParentState{Number: in.Parent.Number, Open: in.Parent.Open, Status: in.Parent.Status}
	}
	if err := praxis.Validate(issue, parent); err != nil {
		var r *praxis.Refusal
		if errors.As(err, &r) {
			return emit(3, map[string]string{"rule": string(r.Rule), "detail": r.Detail, "alarm": r.AlarmKey()})
		}
		return emit(2, map[string]string{"error": err.Error()})
	}
	if args[0] == "validate" {
		return 0
	}
	out, err := praxis.Render(issue, parent)
	if err != nil {
		return emit(2, map[string]string{"error": err.Error()})
	}
	return emit(0, map[string]string{"title": out.Title, "body": out.Body, "claim": praxis.ClaimComment(issue)})
}
