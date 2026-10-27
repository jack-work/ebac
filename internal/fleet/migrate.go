package fleet

import (
	"context"
	"encoding/json"
	"fmt"

	ebacform "github.com/gluck/ebac/internal/form"
	"github.com/jack-work/figaro/api/form"
	"github.com/jack-work/figaro/api/message"
)

func CompactForm(ctx context.Context, st *ebacform.Store, f *ebacform.CriticForm, dry bool) error {
	w, err := f.Critic()
	if err != nil {
		return err
	}
	snap, err := f.Snapshot()
	if err != nil {
		return err
	}
	before, _ := json.Marshal(f.Snap)
	fmt.Printf("%s: form %s v%d is %d B\n", w.Name, f.ID, f.Version, len(before))
	if _, ok := f.Keys["prs"]; ok {
		del := message.Patch{Object: &form.ObjectPatch{Delete: map[string]form.Value{"prs": {}}}}
		raw, _ := json.Marshal(del)
		fmt.Printf("delete patch: %s (%d B)\n", raw, len(raw))
		if len(raw) > 8192 {
			return fmt.Errorf("delete patch is %d B, over 8 KB: aborting", len(raw))
		}
		if dry {
			return nil
		}
		c, err := st.Fig.Node(ctx, f.ID)
		if err != nil {
			return err
		}
		if _, err := c.Set(ctx, del, f.Version); err != nil {
			return fmt.Errorf("delete prs: %w", err)
		}
	} else if dry {
		fmt.Println("no prs to delete")
		return nil
	}
	if err := st.Save(ctx, w, snap, nil); err != nil {
		return fmt.Errorf("write compact prs: %w", err)
	}
	f2, err := st.ReadForm(ctx, f.ID)
	if err != nil {
		return err
	}
	after, _ := json.Marshal(f2.Snap)
	var prs int
	if snap != nil {
		prs = len(snap.PRs)
	}
	fmt.Printf("%s: form %s v%d is %d B (prs %d B, %d PRs)\n", w.Name, f.ID, f2.Version, len(after), len(f2.Keys["prs"]), prs)
	return nil
}
