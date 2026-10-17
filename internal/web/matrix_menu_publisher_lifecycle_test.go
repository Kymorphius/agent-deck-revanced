package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: pause exactly after source context checks and before the
// production annotation reader. Shutdown must remain final even when that
// already-entered annotation loader is released after resource cleanup.
func TestMatrixMenuPublisherShutdownCannotReopenAnnotations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	profile := fmt.Sprintf("publisher-close-%d", time.Now().UnixNano())
	path, err := session.GetDBPathForProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	writeAnnotatedStateDB(t, path, "one", "running")
	srv := NewServer(Config{Profile: profile, MenuData: &fakeMenuDataLoader{snapshot: &MenuSnapshot{
		Profile: profile, Items: []MenuItem{{Type: MenuItemTypeSession, Session: &MenuSession{ID: "one", Status: "running"}}},
	}}})
	srv.hookStatusLoader = nil
	if _, err := srv.annotationReader.load(profile); err != nil {
		t.Fatal(err)
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		srv.menuPublisher.Close()
		srv.annotationReader.close()
	})
	srv.annotationLoader = func(profile string) (map[string]*statedb.InstanceAnnotations, error) {
		close(entered)
		<-gate
		return srv.annotationReader.load(profile)
	}
	done := make(chan error, 1)
	go func() {
		_, err := srv.menuPublisher.LoadMenuSnapshot()
		done <- err
	}()
	<-entered
	srv.menuPublisher.mu.Lock()
	collectionDone := srv.menuPublisher.loading
	srv.menuPublisher.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case <-collectionDone:
	case <-time.After(time.Second):
		t.Fatal("late source collection did not finish")
	}
	if err := <-done; err == nil {
		t.Fatal("canceled lazy request returned success")
	}
	srv.annotationReader.mu.Lock()
	closed, openDB := srv.annotationReader.closed, srv.annotationReader.db != nil
	srv.annotationReader.mu.Unlock()
	if !closed || openDB {
		t.Fatalf("shutdown reader reopened: closed=%v, databaseOpen=%v", closed, openDB)
	}
	if _, err := srv.annotationReader.load(profile); err == nil {
		t.Fatal("closed reader accepted a later read")
	}
}
