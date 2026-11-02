package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"
)

// Explicit live runner only: consumes defaults actually saved by the product
// browser. No settings writes or task snapshot overrides occur in this test.
func TestWorkSettingsBrowserSavedConsumer(t *testing.T) {
	dsn := os.Getenv("BBA_B4_SETTINGS_LIVE_DSN")
	if dsn == "" {
		t.Skip("explicit B4 loopback product fixture runner required")
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	if os.Getenv("BBA_FIXTURE_ONLY") != "1" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "5432" || u.Path != "/bba_fixture" || u.User.Username() != "bba_fixture" {
		t.Fatal("refusing non-owned loopback fixture")
	}
	n, e := strconv.Atoi(os.Getenv("BBA_B4_EXPECT_SLOTS"))
	if e != nil || n < 1 || n > 2 {
		t.Fatal("expected1/2 required")
	}
	s, e := Open(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	saved, e := s.GetWorkSettings(context.Background(), workSettingsActor())
	if e != nil || saved.WorkSlots != n || saved.Source != "workspace" {
		t.Fatal(saved, e)
	}
	id := b4SlotTaskNamed(t, s, fmt.Sprintf("_browser_n%d", n), []string{fmt.Sprintf("903%d101", n), fmt.Sprintf("903%d102", n), fmt.Sprintf("903%d103", n)})
	b4Control(t, s, id, "pause")
	b4Control(t, s, id, "resume")
	barrier := &b4SlotBarrier{entered: make(chan Reservation, 3), release: make(chan struct{})}
	defer b4Close(barrier.release)
	worker := proposalWorkWorker(t, s, barrier, WorkerSteps{Proposal: true})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := worker.Tick(ctx); done <- e }()
	defer func() {
		b4Close(barrier.release)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("consumer did not stop")
		}
	}()
	for i := 0; i < n; i++ {
		r := b4Wait(t, barrier.entered, ctx)
		if r.TaskID != id {
			t.Fatal("unexpected task", r)
		}
	}
	if got := scalarInt(t, s, `SELECT count(*) FROM core.work_item WHERE task_id=$1 AND state='READY'`, id); got != 3-n {
		t.Fatal("preclaimed or missing ready", got)
	}
	select {
	case r := <-barrier.entered:
		t.Fatal("overflow", r)
	default:
	}
	b4Close(barrier.release)
	b4Done(t, done, ctx)
	done <- nil
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.max != n || barrier.active != 0 || len(barrier.events) != 6 {
		t.Fatal(barrier.max, barrier.active, barrier.events)
	}
	if got := scalarInt(t, s, `SELECT work_slots FROM core.task WHERE id=$1`, id); got != n {
		t.Fatal(got)
	}
	if n == 2 && scalarInt(t, s, `SELECT count(*) FROM core.task WHERE title='fixture task' AND work_slots=1`) < 1 {
		t.Fatal("old task lost snapshot")
	}
	data, e := json.Marshal(map[string]any{"source": "browser_saved_workspace_default", "default_version": saved.Version, "task": id, "snapshot": n, "maximum_inflight": barrier.max, "final_inflight": barrier.active, "events": barrier.events, "ready_at_barrier": 3 - n})
	if e != nil {
		t.Fatal(e)
	}
	t.Log(string(data))
}
