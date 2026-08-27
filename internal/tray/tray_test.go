package tray

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/panphora/htmlclay/internal/testutil"
)

func TestIconEmbedded(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"icon.png", iconBytes},
		{"icon-template.png", iconTemplateBytes},
	} {
		if len(c.data) == 0 {
			t.Fatalf("%s not embedded", c.name)
		}
		if c.data[0] != 0x89 || c.data[1] != 'P' || c.data[2] != 'N' || c.data[3] != 'G' {
			t.Fatalf("embedded %s is not a valid PNG", c.name)
		}
	}
}

// Every trusted folder gets a clickable row, however many there are.
//
// The pool used to be fixed at slotCount, and a row click is untrustFolder's only
// caller, so entries past the sixteenth stayed trusted with no way to revoke them
// from the menu. Asserting "every row has a label" would have passed the whole
// time; the click is the part that was missing, so drive one.
func TestEveryRowIsClickableBeyondTheInitialPool(t *testing.T) {
	lm, _ := newListMenu("Trusted Folders", "", "No folders yet", "", "")

	const count = slotCount + 20
	rows := make([]Row, count)
	for i := range rows {
		rows[i] = Row{Path: fmt.Sprintf("/home/me/project-%02d", i), Label: fmt.Sprintf("project-%02d", i)}
	}

	clicked := make(chan string, 1)
	lm.watch(func(path string) []Row {
		clicked <- path
		return nil
	}, nil)
	lm.render(rows)

	lm.mu.Lock()
	placed := map[string]*slot{}
	for _, s := range lm.slots {
		if s.path != "" {
			placed[s.path] = s
		}
	}
	lm.mu.Unlock()
	if len(placed) != count {
		t.Fatalf("%d of %d rows got a slot", len(placed), count)
	}

	// The last row is the one a fixed pool could never place.
	last := rows[count-1]
	placed[last.Path].item.ClickedCh <- struct{}{}
	got := testutil.Receive(t, 10*time.Second, "a row click to reach the remove hook", clicked)
	if got != last.Path {
		t.Errorf("clicking the last row reported %q, want %q", got, last.Path)
	}
}

// Growing the pool must not move an entry that is already placed. A click can sit
// queued against a row while a render runs, and a slot whose meaning shifted would
// untrust a different folder than the one the user clicked.
func TestGrowingThePoolLeavesPlacedEntriesWhereTheyAre(t *testing.T) {
	lm, _ := newListMenu("Trusted Folders", "", "No folders yet", "", "")

	first := make([]Row, slotCount)
	for i := range first {
		first[i] = Row{Path: fmt.Sprintf("/home/me/first-%02d", i), Label: "first"}
	}
	lm.render(first)

	lm.mu.Lock()
	before := make([]string, len(lm.slots))
	for i, s := range lm.slots {
		before[i] = s.path
	}
	lm.mu.Unlock()

	lm.render(append(append([]Row(nil), first...), Row{Path: "/home/me/one-more", Label: "one more"}))

	lm.mu.Lock()
	defer lm.mu.Unlock()
	if len(lm.slots) <= len(before) {
		t.Fatalf("the pool should have grown past %d, got %d", len(before), len(lm.slots))
	}
	for i, path := range before {
		if lm.slots[i].path != path {
			t.Errorf("slot %d moved from %q to %q; a queued click would land on the wrong folder", i, path, lm.slots[i].path)
		}
	}
}

// The notice row is how a Linux desktop with no permission dialogs says so, and
// it is also the row that must not appear anywhere else. An empty notice adding
// a blank disabled row would put one at the top of every menu on every platform.
func TestNoticeRowAppearsOnlyWhenThereIsSomethingToSay(t *testing.T) {
	if item := (&Tray{}).addNotice(); item != nil {
		t.Errorf("an empty notice added a row: %s", item)
	}

	const notice = "HTML Clay cannot show permission dialogs on this desktop. Install zenity or kdialog."
	item := (&Tray{notice: notice}).addNotice()
	if item == nil {
		t.Fatal("a notice must get a row of its own")
	}
	if !strings.Contains(item.String(), notice) {
		t.Errorf("the row does not carry the notice: %s", item)
	}
	if !item.Disabled() {
		t.Error("the notice row must be disabled; there is nothing behind it to click")
	}
}
