package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// ADR-L9: a successful live fetch is not success until the sync saves it.
// Reuse the synthetic, component-built identities from the live-info fixtures.
func TestGroupsInfoPersist01ClosedStoreRejectsSuccess(t *testing.T) {
	db := openGroupDelegateTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	client := &fakeGroupInfoClient{info: liveInfoFixture()}
	resp, err := executeDelegatedGroupInfo(context.Background(), client, db, sendDelegateRequest{
		Version: sendDelegateVersion,
		Kind:    groupInfoKind,
		To:      liveInfoGroup(),
	})
	calls := client.snapshot()
	if len(calls) != 1 || calls[0].String() != liveInfoGroup() {
		t.Fatalf("valid live lookup must precede the save failure: calls=%v", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "not saved to the local store") {
		t.Errorf("save failure must propagate with local-store context: err=%v", err)
	}
	if resp.OK {
		t.Error("save failure must not return sendDelegateResponse.OK=true")
	}
}

func TestGroupsInfoPersist02ClosedStoreCLIFailsWithoutGroupJSON(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	t.Setenv("WACLI_READONLY", "")
	db := openGroupDelegateTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	client := &fakeGroupInfoClient{info: liveInfoFixture()}
	dir := startLockedGroupDelegate(t, liveInfoExecutor(client, db))
	stdout, stderr, err := runPresenceDelegateHelper(t, liveInfoArgs(dir))
	calls := client.snapshot()
	if len(calls) != 1 || calls[0].String() != liveInfoGroup() {
		t.Fatalf("CLI must reach the real delegate and valid live lookup once: calls=%v", calls)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Errorf("save failure must make groups info exit nonzero: err=%v", err)
	}
	if !strings.Contains(stdout+stderr, "not saved to the local store") {
		t.Errorf("CLI must explain the failed local save: stdout=%q stderr=%q", stdout, stderr)
	}
	if commandStdout(stdout) != "" {
		t.Errorf("save failure must not print group JSON: stdout=%q", commandStdout(stdout))
	}
}
