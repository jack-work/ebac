package poll

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gluck/ebac/internal/form"
	"github.com/gluck/ebac/internal/model"
	"github.com/gluck/ebac/internal/trigger"
)

// Classify is the core's decision for every source: the critic's trigger
// rules set each event's tier or drop it. Every source calls it on a round's
// fresh events before they join the pending batch.
//
// A broken rule set never silences or fails a round: the last good set stays
// in force (else the source's default set) and the parse error is written to
// the form key trigger_error, which the first good set removes.
func (p *Poller) Classify(ctx context.Context, w *model.Critic, events []model.Event) (kept, dropped []model.Event) {
	rules, errText := Rules(w)
	if errText != w.Runtime.TriggerErr {
		if err := p.Store.Commit(ctx, w.FormID, func(map[string]json.RawMessage, form.EbacState) (map[string]json.RawMessage, []string) {
			if errText == "" {
				return nil, []string{trigger.ErrorKey}
			}
			return map[string]json.RawMessage{trigger.ErrorKey: model.MustJSON(errText)}, nil
		}); err != nil {
			fmt.Fprintf(p.Out, "%s: could not write %s: %v\n", w.Name, trigger.ErrorKey, err)
		} else {
			w.Runtime.TriggerErr = errText
		}
		if errText != "" {
			fmt.Fprintf(p.Out, "%s: %s\n", w.Name, errText)
		}
	}
	if len(events) == 0 {
		return nil, nil
	}
	return rules.Apply(events)
}

// Rules is the set in force for a critic, and the error to show on the form
// ("" when its trigger key is absent or valid). It records a valid set as the
// last good one.
func Rules(w *model.Critic) (trigger.Rules, string) {
	if len(w.Trigger) == 0 {
		w.Runtime.TriggerGood = nil
		return trigger.DefaultFor(w.SourceName()), ""
	}
	rules, err := trigger.Parse(w.Trigger)
	if err == nil {
		w.Runtime.TriggerGood = append(json.RawMessage(nil), w.Trigger...)
		return rules, ""
	}
	in := "the source default set"
	fallback := trigger.DefaultFor(w.SourceName())
	if good, gerr := trigger.Parse(w.Runtime.TriggerGood); gerr == nil {
		fallback, in = good, "the last good set"
	}
	return fallback, fmt.Sprintf("trigger rejected, %s is in force: %v", in, err)
}

// operatorName is the Boss's Teams display name, as older critics listed him
// in chat.quiet. The default rules record him by id, so migration drops it.
const operatorName = "Jack Kelliher"

// migrateQuiet turns a pre-trigger chat.quiet list into rules on the role
// form, once: the Teams default set with every quiet sender but the operator
// recorded before the final default. A form that already has rules keeps
// them. The legacy field is not written back, so it is gone after this round.
func (p *Poller) migrateQuiet(ctx context.Context, w *model.Critic) error {
	if len(w.LegacyQuiet) == 0 || len(w.Trigger) > 0 {
		return nil
	}
	rules := trigger.MigrateQuiet(w.LegacyQuiet, operatorName)
	raw := rules.JSON()
	if err := p.Store.Commit(ctx, w.FormID, func(cur map[string]json.RawMessage, _ form.EbacState) (map[string]json.RawMessage, []string) {
		if len(cur[trigger.FormKey]) > 0 {
			return nil, nil // set by the role holder since the read
		}
		return map[string]json.RawMessage{trigger.FormKey: raw}, nil
	}); err != nil {
		return fmt.Errorf("migrate quiet %v to %s: %w", w.LegacyQuiet, trigger.FormKey, err)
	}
	fmt.Fprintf(p.Out, "%s: migrated quiet %v to %s rules\n", w.Name, w.LegacyQuiet, trigger.FormKey)
	w.Trigger, w.LegacyQuiet = raw, nil
	return nil
}
