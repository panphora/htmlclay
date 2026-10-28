package server

import (
	"testing"
)

func TestRunningWorkCountsAIEditsAndHelperRequests(t *testing.T) {
	srv, f := setupLiveSyncTest(t)
	if n := srv.RunningWork(); n != 0 {
		t.Fatalf("an idle server reports %d running", n)
	}
	srv.aiEdit.mu.Lock()
	srv.aiEdit.running = map[string]aiEditRun{f.AbsPath: {id: "e1", cancel: func() {}}}
	srv.aiEdit.mu.Unlock()
	fake := &helperDispatcher{live: map[string]liveHelperRun{"r1": {cancel: func() {}}, "r2": {cancel: func() {}}}}
	srv.helperMu.Lock()
	if srv.helpers == nil {
		srv.helpers = map[string]*helperDispatcher{}
	}
	srv.helpers["fake-document"] = fake
	srv.helperMu.Unlock()
	t.Cleanup(func() {
		srv.helperMu.Lock()
		delete(srv.helpers, "fake-document")
		srv.helperMu.Unlock()
		srv.aiEdit.mu.Lock()
		srv.aiEdit.running = nil
		srv.aiEdit.mu.Unlock()
	})
	if n := srv.RunningWork(); n != 3 {
		t.Fatalf("RunningWork = %d, want 3 (one AI edit, two helper requests)", n)
	}
}
