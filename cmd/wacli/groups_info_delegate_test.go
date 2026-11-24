package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/delegate"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// Synthetic, non-phone identities; separate components avoid email-shaped PII
// false positives without introducing real WhatsApp identifiers (R-L12).
func liveInfoJID(user, server string) string { return user + "@" + server }
func liveInfoGroup() string                  { return liveInfoJID("toy-group", "g.us") }

type fakeGroupInfoClient struct {
	mu    sync.Mutex
	calls []types.JID
	info  *types.GroupInfo
	err   error
}

func (f *fakeGroupInfoClient) ResolveLIDToPN(_ context.Context, jid types.JID) types.JID { return jid }
func (f *fakeGroupInfoClient) GetGroupInfo(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, jid)
	return f.info, f.err
}
func (f *fakeGroupInfoClient) snapshot() []types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.JID(nil), f.calls...)
}
func liveInfoFixture() *types.GroupInfo {
	return &types.GroupInfo{
		JID:              types.NewJID("toy-group", types.GroupServer),
		OwnerJID:         types.NewJID("toy-owner", types.DefaultUserServer),
		GroupName:        types.GroupName{Name: "Fresh toy group"},
		GroupCreated:     time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		ParticipantCount: 2,
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("toy-owner", types.DefaultUserServer), IsSuperAdmin: true},
			{JID: types.NewJID("toy-new-member", types.DefaultUserServer), IsAdmin: true},
		},
	}
}
func liveInfoExecutor(client *fakeGroupInfoClient, db *store.DB) sendDelegateExecutor {
	return func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if req.Version != sendDelegateVersion || req.Kind != groupInfoKind {
			return sendDelegateResponse{}, fmt.Errorf("unexpected request version=%d kind=%q", req.Version, req.Kind)
		}
		return executeDelegatedGroupInfo(ctx, client, db, req)
	}
}
func liveInfoArgs(dir string) []string {
	return []string{"--store", dir, "--json", "--timeout", "2s", "groups", "info", "--jid", liveInfoGroup()}
}
func assertLiveInfoJSON(t *testing.T, stdout string, info *types.GroupInfo) {
	t.Helper()
	var want bytes.Buffer
	if err := out.WriteJSON(&want, info); err != nil {
		t.Fatal(err)
	}
	if commandStdout(stdout) != want.String() {
		t.Fatalf("JSON compatibility: got %q want %q", commandStdout(stdout), want.String())
	}
}
func liveInfoSeed(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.UpsertGroup(liveInfoGroup(), "Stale toy group", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceGroupParticipants(liveInfoGroup(), []store.GroupParticipant{{GroupJID: liveInfoGroup(), UserJID: liveInfoJID("toy-old-member", "s.whatsapp.net"), Role: "member"}}); err != nil {
		t.Fatal(err)
	}
}

func TestGroupsInfoLive01LockedDelegationAndJSON(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	client := &fakeGroupInfoClient{info: liveInfoFixture()}
	db := openGroupDelegateTestDB(t)
	dir := startLockedGroupDelegate(t, liveInfoExecutor(client, db))
	stdout, stderr, err := runPresenceDelegateHelper(t, liveInfoArgs(dir))
	if err != nil {
		t.Fatalf("live groups info must delegate: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	calls := client.snapshot()
	if len(calls) != 1 || calls[0].String() != liveInfoGroup() {
		t.Fatalf("GetGroupInfo calls = %v, want exactly the requested group", calls)
	}
	assertLiveInfoJSON(t, stdout, client.info)
}

func TestGroupsInfoLive02PersistsFreshParticipants(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	db := openGroupDelegateTestDB(t)
	liveInfoSeed(t, db)
	client := &fakeGroupInfoClient{info: liveInfoFixture()}
	dir := startLockedGroupDelegate(t, liveInfoExecutor(client, db))
	resp, err := delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 2 * time.Second}, sendDelegateRequest{Kind: groupInfoKind, To: liveInfoGroup()})
	if err != nil || !resp.OK {
		t.Fatalf("live server lookup: response=%+v err=%v", resp, err)
	}
	if len(client.snapshot()) != 1 {
		t.Fatal("success without GetGroupInfo")
	}
	groups, err := db.ListGroups("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != client.info.GroupName.Name || groups[0].OwnerJID != client.info.OwnerJID.String() {
		t.Fatalf("fresh group not persisted: %+v", groups)
	}
	ps, err := db.ListGroupParticipants(liveInfoGroup())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range ps {
		got[p.UserJID] = p.Role
	}
	want := map[string]string{client.info.Participants[0].JID.String(): "superadmin", client.info.Participants[1].JID.String(): "admin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("participants=%v want=%v (stale participant must disappear)", got, want)
	}
}

func TestGroupsInfoLive03ReadOnlyDelegates(t *testing.T) {
	for _, mode := range []string{"flag", "environment"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WACLI_READONLY", "")
			client := &fakeGroupInfoClient{info: liveInfoFixture()}
			dir := startLockedGroupDelegate(t, liveInfoExecutor(client, openGroupDelegateTestDB(t)))
			args := liveInfoArgs(dir)
			if mode == "flag" {
				args = append([]string{"--read-only"}, args...)
			} else {
				t.Setenv("WACLI_READONLY", "1")
			}
			stdout, stderr, err := runPresenceDelegateHelper(t, args)
			if err != nil {
				t.Fatalf("read-only live query must delegate: %v stdout=%q stderr=%q", err, stdout, stderr)
			}
			if len(client.snapshot()) != 1 {
				t.Fatal("read-only query did not reach live client exactly once")
			}
			assertLiveInfoJSON(t, stdout, client.info)
			if _, err := os.Stat(filepath.Join(dir, "session.db")); !os.IsNotExist(err) {
				t.Fatalf("CLI created/opened session store: %v", err)
			}
		})
	}
}

func TestGroupsInfoLive04ReadOnlyWithoutSyncNeverAuthenticates(t *testing.T) {
	for _, mode := range []string{"flag", "environment"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("WACLI_READONLY", "")
			dir := shortPresenceDelegateStoreDir(t)
			args := liveInfoArgs(dir)
			if mode == "flag" {
				args = append([]string{"--read-only"}, args...)
			} else {
				t.Setenv("WACLI_READONLY", "1")
			}
			stdout, stderr, err := runPresenceDelegateHelper(t, args)
			text := strings.ToLower(stdout + stderr)
			if err == nil || !strings.Contains(text, "live query needs a running sync") {
				t.Errorf("want explicit live-unavailable failure: err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			for _, forbidden := range []string{"not authenticated", "session", "auth"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("unexpected authentication/session path: %q", text)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "session.db")); !os.IsNotExist(err) {
				t.Errorf("session.db must not exist: %v", err)
			}
		})
	}
}

func TestGroupsInfoLive05RejectsInvalidJIDsBeforeClient(t *testing.T) {
	for name, jid := range map[string]string{"user": liveInfoJID("toy-user", "s.whatsapp.net"), "empty": "", "garbage": "not-a-jid"} {
		t.Run(name, func(t *testing.T) {
			client := &fakeGroupInfoClient{info: liveInfoFixture()}
			received := make(chan sendDelegateRequest, 4)
			dir := startLockedGroupDelegate(t, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
				received <- req
				return liveInfoExecutor(client, openDBUnused)(ctx, req)
			})
			args := liveInfoArgs(dir)
			args[len(args)-1] = jid
			stdout, stderr, err := runPresenceDelegateHelper(t, args)
			text := strings.ToLower(stdout + stderr)
			if err == nil || !(strings.Contains(text, "jid") || strings.Contains(text, "group")) || strings.Contains(text, "locked") {
				t.Errorf("want client-side JID rejection, got %v stdout=%q stderr=%q", err, stdout, stderr)
			}
			select {
			case req := <-received:
				t.Errorf("invalid CLI JID reached socket: %+v", req)
			default:
			}
			// Hand-written requests exercise the server independently of CLI validation.
			_, err = delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 2 * time.Second}, sendDelegateRequest{Kind: groupInfoKind, To: jid})
			if err == nil || !(strings.Contains(strings.ToLower(err.Error()), "jid") || strings.Contains(strings.ToLower(err.Error()), "group")) || strings.Contains(err.Error(), "not implemented") {
				t.Errorf("want server JID validation, got %v", err)
			}
			if len(client.snapshot()) != 0 {
				t.Errorf("invalid JID reached WhatsApp: %v", client.snapshot())
			}
		})
	}
}

// No DB is needed for invalid requests: validation must precede persistence.
var openDBUnused *store.DB

func TestGroupsInfoLive06LookupErrorNoFallbackOrWrites(t *testing.T) {
	db := openGroupDelegateTestDB(t)
	liveInfoSeed(t, db)
	beforeGroups, err := db.ListGroups("", 10)
	if err != nil {
		t.Fatal(err)
	}
	beforeParticipants, err := db.ListGroupParticipants(liveInfoGroup())
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeGroupInfoClient{err: errors.New("toy live lookup unavailable")}
	dir := startLockedGroupDelegate(t, liveInfoExecutor(client, db))
	stdout, stderr, err := runPresenceDelegateHelper(t, liveInfoArgs(dir))
	if err == nil || !strings.Contains(stderr, "toy live lookup unavailable") {
		t.Errorf("must propagate live error, not cached success: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if commandStdout(stdout) != "" {
		t.Errorf("failure emitted fallback data: %q", stdout)
	}
	if len(client.snapshot()) != 1 {
		t.Errorf("live lookup calls=%d want=1", len(client.snapshot()))
	}
	afterGroups, err := db.ListGroups("", 10)
	if err != nil {
		t.Fatal(err)
	}
	afterParticipants, err := db.ListGroupParticipants(liveInfoGroup())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeGroups, afterGroups) || !reflect.DeepEqual(beforeParticipants, afterParticipants) {
		t.Fatal("failed lookup changed local snapshot")
	}
}

func TestGroupsInfoLive07OlderDaemonRestartHint(t *testing.T) {
	dir := startLockedGroupDelegate(t, func(_ context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	})
	stdout, stderr, err := runPresenceDelegateHelper(t, liveInfoArgs(dir))
	if err == nil || !strings.Contains(stderr, "restart `wacli sync`") || !strings.Contains(stderr, "groups info") {
		t.Fatalf("want actionable daemon restart hint: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestGroupsInfoLive08UnlockedKeepsDirectAuthBoundary(t *testing.T) {
	// The concrete newApp/Connect path has no injected client (groups_info_rename.go).
	// A fresh unauthenticated store safely observes the existing direct auth boundary;
	// it cannot reach WhatsApp and is not proof of a successful direct network lookup.
	t.Setenv("WACLI_READONLY", "")
	dir := shortPresenceDelegateStoreDir(t)
	stdout, stderr, err := runPresenceDelegateHelper(t, liveInfoArgs(dir))
	if err == nil || !strings.Contains(strings.ToLower(stdout+stderr), "not authenticated") {
		t.Fatalf("unlocked writable command lost direct auth path: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestGroupsInfoLive09QueuedDeadlineNeverDispatches(t *testing.T) {
	dir := shortPresenceDelegateStoreDir(t)
	started := make(chan struct{})
	release := make(chan struct{})
	dispatched := make(chan struct{}, 1)
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		if req.Kind == "text" {
			close(started)
			select {
			case <-release:
				return sendDelegateResponse{OK: true}, nil
			case <-ctx.Done():
				return sendDelegateResponse{}, ctx.Err()
			}
		}
		dispatched <- struct{}{}
		return sendDelegateResponse{OK: true, Group: liveInfoFixture()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := delegateSend(context.Background(), &rootFlags{storeDir: dir, timeout: 3 * time.Second}, sendDelegateRequest{Kind: "text"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking send never started")
	}
	conn, err := delegate.Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	req := sendDelegateRequest{Version: sendDelegateVersion, Kind: groupInfoKind, To: liveInfoGroup(), TimeoutMS: 5000, DeadlineUnixMS: time.Now().Add(150 * time.Millisecond).UnixMilli()}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp sendDelegateResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "it was not sent") {
		t.Fatalf("expired live query must be refused before dispatch: %+v", resp)
	}
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-dispatched:
		t.Fatal("expired live query reached executor")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestGroupsInfoLive09CommandTimeout(t *testing.T) {
	reached := make(chan struct{}, 1)
	dir := startLockedGroupDelegate(t, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		reached <- struct{}{}
		<-ctx.Done()
		return sendDelegateResponse{}, ctx.Err()
	})
	args := liveInfoArgs(dir)
	args[4] = "700ms"
	start := time.Now()
	stdout, stderr, err := runPresenceDelegateHelper(t, args)
	if err == nil || !(strings.Contains(strings.ToLower(stderr), "deadline") || strings.Contains(strings.ToLower(stderr), "timeout")) {
		t.Errorf("want delegation timeout, got %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("caller timeout exceeded bounded budget")
	}
	select {
	case <-reached:
	default:
		t.Error("timeout did not exercise delegation")
	}
}
