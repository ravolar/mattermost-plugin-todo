package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type memoryKVAPI struct {
	plugintest.API

	mu   sync.Mutex
	data map[string][]byte
}

type gatedListCASAPI struct {
	*memoryKVAPI

	gateMu    sync.Mutex
	gateKey   string
	gateCount int
	arrived   int
	gate      chan struct{}
}

type afterListCASGateAPI struct {
	*memoryKVAPI

	gateMu  sync.Mutex
	gateKey string
	reached chan struct{}
	release chan struct{}
	enabled bool
}

func (a *afterListCASGateAPI) enableAfterCASGate(key string) (<-chan struct{}, chan<- struct{}) {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	a.gateKey = key
	a.reached = make(chan struct{})
	a.release = make(chan struct{})
	a.enabled = true
	return a.reached, a.release
}

func (a *afterListCASGateAPI) KVCompareAndSet(key string, oldValue, newValue []byte) (bool, *model.AppError) {
	ok, appErr := a.memoryKVAPI.KVCompareAndSet(key, oldValue, newValue)

	a.gateMu.Lock()
	shouldWait := ok && a.enabled && key == a.gateKey
	reached := a.reached
	release := a.release
	if shouldWait {
		a.enabled = false
	}
	a.gateMu.Unlock()

	if shouldWait {
		close(reached)
		<-release
	}
	return ok, appErr
}

func (a *gatedListCASAPI) enableCASGate(key string, count int) {
	a.gateMu.Lock()
	defer a.gateMu.Unlock()
	a.gateKey = key
	a.gateCount = count
	a.arrived = 0
	a.gate = make(chan struct{})
}

func (a *gatedListCASAPI) KVCompareAndSet(key string, oldValue, newValue []byte) (bool, *model.AppError) {
	a.gateMu.Lock()
	gate := a.gate
	shouldWait := gate != nil && key == a.gateKey && a.arrived < a.gateCount
	if shouldWait {
		a.arrived++
		if a.arrived == a.gateCount {
			close(gate)
			a.gate = nil
		}
	}
	a.gateMu.Unlock()

	if shouldWait {
		<-gate
	}
	return a.memoryKVAPI.KVCompareAndSet(key, oldValue, newValue)
}

func newMemoryKVAPI() *memoryKVAPI {
	return &memoryKVAPI{data: map[string][]byte{}}
}

func (a *memoryKVAPI) KVGet(key string) ([]byte, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()

	value, ok := a.data[key]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), value...), nil
}

func (a *memoryKVAPI) KVSet(key string, value []byte) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.data[key] = append([]byte(nil), value...)
	return nil
}

func (a *memoryKVAPI) KVDelete(key string) *model.AppError {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.data, key)
	return nil
}

func (a *memoryKVAPI) KVCompareAndSet(key string, oldValue, newValue []byte) (bool, *model.AppError) {
	a.mu.Lock()
	defer a.mu.Unlock()

	current, exists := a.data[key]
	matches := (!exists && oldValue == nil) || (exists && bytes.Equal(current, oldValue))
	if !matches {
		return false, nil
	}
	a.data[key] = append([]byte(nil), newValue...)
	return true, nil
}

func (a *memoryKVAPI) countKeysWithPrefix(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	count := 0
	for key := range a.data {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

func TestAddIssueClientRequestIDConcurrentReplayCreatesOnePhysicalIssue(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)

	const replays = 32
	start := make(chan struct{})
	issues := make(chan *Issue, replays)
	errs := make(chan error, replays)
	var wg sync.WaitGroup
	for range replays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			issue, err := manager.AddIssue("owner", "one title", "", "one description", "", "request-1")
			issues <- issue
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(issues)
	close(errs)

	var issueID string
	freshResults := 0
	for err := range errs {
		require.NoError(t, err)
	}
	for issue := range issues {
		require.NotNil(t, issue)
		if issueID == "" {
			issueID = issue.ID
		}
		assert.Equal(t, issueID, issue.ID)
		assert.Equal(t, "request-1", issue.ClientRequestID)
		if !issue.Replay {
			freshResults++
		}
		assert.False(t, issue.TerminalReplay)
	}
	assert.Equal(t, 1, freshResults, "exactly one concurrent add owns the fresh result")

	lists, err := manager.GetAllList("owner")
	require.NoError(t, err)
	require.Len(t, lists.My, 1)
	assert.Equal(t, issueID, lists.My[0].ID)
	assert.Equal(t, "request-1", lists.My[0].ClientRequestID)
	assert.Equal(t, 1, api.countKeysWithPrefix(StoreIssueKey+"_"))
	assert.Equal(t, 1, api.countKeysWithPrefix(StoreAddResultKey+"_"))

	encoded, err := json.Marshal(lists)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"client_request_id":"request-1"`)
}

func TestRemoveReferenceIfExistsCleansEveryExactDuplicate(t *testing.T) {
	api := newMemoryKVAPI()
	store := NewListStore(api)
	references := []*IssueRef{
		{IssueID: "target"},
		{IssueID: "target", ForeignUserID: "legacy", ForeignIssueID: "legacy-copy"},
		{IssueID: "unrelated"},
	}
	encoded, err := json.Marshal(references)
	require.NoError(t, err)
	require.Nil(t, api.KVSet(listKey("owner", MyListKey), encoded))

	require.NoError(t, store.RemoveReferenceIfExists("owner", "target", MyListKey))
	remaining, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "unrelated", remaining[0].IssueID)
}

func TestCommittedLedgerWithoutIssueBecomesTerminalReplay(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	store := NewListStore(api)
	created, err := manager.AddIssue("owner", "title", "", "description", "", "committed-missing-issue")
	require.NoError(t, err)
	require.NoError(t, store.RemoveIssue(created.ID))

	replayed, err := manager.AddIssue("owner", "title", "", "description", "", "committed-missing-issue")
	require.NoError(t, err)
	require.NotNil(t, replayed)
	assert.Equal(t, created.ID, replayed.ID)
	assert.True(t, replayed.Replay)
	assert.True(t, replayed.TerminalReplay)
	remaining, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	assert.Empty(t, remaining)
	ledger, _, err := store.GetAddResult(created.ID)
	require.NoError(t, err)
	require.NotNil(t, ledger)
	assert.Equal(t, addResultStateTerminal, ledger.State)
}

func TestReservedAddRefusesDeterministicIssueCollision(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	store := NewListStore(api)
	candidate := newIdempotentIssue("owner", "title", "", "description", "", "collision-request")
	conflicting := *candidate
	conflicting.ClientRequestID = "different-request"
	require.NoError(t, store.SaveIssue(&conflicting))

	created, err := manager.AddIssue("owner", "title", "", "description", "", "collision-request")
	assert.Nil(t, created)
	require.ErrorIs(t, err, errClientRequestIDConflict)
	remaining, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestTerminalActionCreatesMissingLedgerTombstone(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api).(*listManager)
	store := NewListStore(api)
	issue := newIdempotentIssue("owner", "title", "", "description", "", "missing-ledger")
	require.NoError(t, store.SaveIssue(issue))
	require.NoError(t, store.EnsureReference("owner", issue.ID, MyListKey, "", ""))

	require.NoError(t, manager.markAddResultTerminal("owner", issue))
	ledger, _, err := store.GetAddResult(issue.ID)
	require.NoError(t, err)
	require.NotNil(t, ledger)
	assert.Equal(t, addResultStateTerminal, ledger.State)
	assert.Equal(t, issue.ClientRequestID, ledger.ClientRequestID)
}

func TestDeletedIdempotentAddReplaysImmutableResultWithoutRecreation(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)

	created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	_, _, _, err = manager.EditIssue("owner", created.ID, "edited title", "edited description")
	require.NoError(t, err)
	removed, _, _, _, err := manager.RemoveIssue("owner", created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, removed.ID)
	assert.Equal(t, "edited title", removed.Message)

	replayed, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	assert.Equal(t, created.ID, replayed.ID)
	assert.Equal(t, "request-1", replayed.ClientRequestID)
	assert.True(t, replayed.Replay)
	assert.True(t, replayed.TerminalReplay)

	lists, err := manager.GetAllList("owner")
	require.NoError(t, err)
	assert.Empty(t, lists.My)
	assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"))
	assert.Equal(t, 1, api.countKeysWithPrefix(StoreAddResultKey+"_"))

	result, _, err := NewListStore(api).GetAddResult(created.ID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, addResultStateTerminal, result.State)
	assert.Equal(t, created.ID, result.IssueID)
	assert.Equal(t, "request-1", result.ClientRequestID)
	assert.NotEmpty(t, result.PayloadSHA256)
	assert.NotZero(t, result.TerminalAt)

	conflict, err := manager.AddIssue("owner", "changed", "", "description", "", "request-1")
	assert.Nil(t, conflict)
	assert.True(t, errors.Is(err, errClientRequestIDConflict))
	assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"))
}

func TestCorrelatedAddAssignmentFailsClosedDuringConcurrentReplay(t *testing.T) {
	for iteration := range 50 {
		api := newMemoryKVAPI()
		manager := NewListManager(api)
		created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
		require.NoError(t, err)

		start := make(chan struct{})
		assignmentErr := make(chan error, 1)
		replayResult := make(chan *Issue, 1)
		replayErr := make(chan error, 1)
		go func() {
			<-start
			_, _, _, err := manager.ChangeAssignment(created.ID, "owner", "receiver")
			assignmentErr <- err
		}()
		go func() {
			<-start
			issue, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
			replayResult <- issue
			replayErr <- err
		}()
		close(start)

		require.ErrorIs(t, <-assignmentErr, errCorrelatedIssueAssignmentUnsupported, "iteration %d", iteration)
		require.NoError(t, <-replayErr, "iteration %d", iteration)
		replayed := <-replayResult
		require.NotNil(t, replayed, "iteration %d", iteration)
		assert.Equal(t, created.ID, replayed.ID, "iteration %d", iteration)
		assert.True(t, replayed.Replay, "iteration %d", iteration)
		assert.False(t, replayed.TerminalReplay, "iteration %d", iteration)

		store := NewListStore(api)
		ownerMy, err := store.GetList("owner", MyListKey)
		require.NoError(t, err, "iteration %d", iteration)
		require.Len(t, ownerMy, 1, "iteration %d", iteration)
		assert.Equal(t, created.ID, ownerMy[0].IssueID, "iteration %d", iteration)
		ownerOut, err := store.GetList("owner", OutListKey)
		require.NoError(t, err, "iteration %d", iteration)
		assert.Empty(t, ownerOut, "iteration %d", iteration)
		receiverIn, err := store.GetList("receiver", InListKey)
		require.NoError(t, err, "iteration %d", iteration)
		assert.Empty(t, receiverIn, "iteration %d", iteration)
		_, exists, err := store.GetIssueIfExists(created.ID)
		require.NoError(t, err, "iteration %d", iteration)
		assert.True(t, exists, "iteration %d", iteration)

		result, _, err := store.GetAddResult(created.ID)
		require.NoError(t, err, "iteration %d", iteration)
		require.NotNil(t, result, "iteration %d", iteration)
		assert.Equal(t, addResultStateCommitted, result.State, "iteration %d", iteration)
		assert.Equal(t, 1, api.countKeysWithPrefix(StoreIssueKey+"_"), "iteration %d", iteration)
	}
}

func TestCorrelatedAssignmentGuardPreservesForeignAuthorizationBoundary(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)

	foreignIssue, oldOwner, receiverIssueID, err := manager.ChangeAssignment(created.ID, "foreign-user", "receiver")
	assert.Nil(t, foreignIssue)
	assert.Empty(t, oldOwner)
	assert.Empty(t, receiverIssueID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errCorrelatedIssueAssignmentUnsupported)
	assert.EqualError(t, err, "reference not found")

	ownerIssue, oldOwner, receiverIssueID, err := manager.ChangeAssignment(created.ID, "owner", "receiver")
	assert.Nil(t, ownerIssue)
	assert.Empty(t, oldOwner)
	assert.Empty(t, receiverIssueID)
	require.ErrorIs(t, err, errCorrelatedIssueAssignmentUnsupported)

	store := NewListStore(api)
	ownerMy, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	require.Len(t, ownerMy, 1)
	assert.Equal(t, created.ID, ownerMy[0].IssueID)
	for _, userID := range []string{"owner", "foreign-user", "receiver"} {
		for _, listID := range []string{InListKey, OutListKey} {
			references, err := store.GetList(userID, listID)
			require.NoError(t, err)
			assert.Empty(t, references, "user %q list %q changed", userID, listID)
		}
	}
	_, exists, err := store.GetIssueIfExists(created.ID)
	require.NoError(t, err)
	assert.True(t, exists)
	ledger, _, err := store.GetAddResult(created.ID)
	require.NoError(t, err)
	require.NotNil(t, ledger)
	assert.Equal(t, addResultStateCommitted, ledger.State)
	assert.Equal(t, 1, api.countKeysWithPrefix(StoreIssueKey+"_"))
}

func TestLegacyChangeAssignmentWithoutClientRequestIDPreservesBehavior(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	created, err := manager.AddIssue("owner", "legacy title", "", "description", "", "")
	require.NoError(t, err)

	assigned, oldOwner, receiverIssueID, err := manager.ChangeAssignment(created.ID, "owner", "receiver")
	require.NoError(t, err)
	assert.Equal(t, created.ID, assigned.ID)
	assert.Empty(t, oldOwner)
	assert.NotEmpty(t, receiverIssueID)
	assert.NotEqual(t, created.ID, receiverIssueID)

	store := NewListStore(api)
	ownerMy, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	assert.Empty(t, ownerMy)
	ownerOut, err := store.GetList("owner", OutListKey)
	require.NoError(t, err)
	require.Len(t, ownerOut, 1)
	assert.Equal(t, &IssueRef{
		IssueID:        created.ID,
		ForeignIssueID: receiverIssueID,
		ForeignUserID:  "receiver",
	}, ownerOut[0])
	receiverIn, err := store.GetList("receiver", InListKey)
	require.NoError(t, err)
	require.Len(t, receiverIn, 1)
	assert.Equal(t, &IssueRef{
		IssueID:        receiverIssueID,
		ForeignIssueID: created.ID,
		ForeignUserID:  "owner",
	}, receiverIn[0])
	assert.Equal(t, 2, api.countKeysWithPrefix(StoreIssueKey+"_"))
}

func TestTerminalReplayCleansExactIssueResidueFromAllOwnerLists(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	concreteManager := manager.(*listManager)
	store := NewListStore(api)

	created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	receiverIssue := newIssue(created.Message, created.PostPermalink, created.Description, created.PostID)
	require.NoError(t, store.SaveIssue(receiverIssue))
	require.NoError(t, store.RemoveReference("owner", created.ID, MyListKey))
	require.NoError(t, store.AddReference("owner", created.ID, OutListKey, "receiver", receiverIssue.ID))
	require.NoError(t, store.AddReference("receiver", receiverIssue.ID, InListKey, "owner", created.ID))
	// Model a historical assigned residue plus a second exact owner-list residue.
	// The correlated ChangeAssignment path is now blocked before mutation, but a
	// terminal replay must still clean a pre-release topology without guessing.
	require.NoError(t, store.EnsureReference("owner", created.ID, InListKey, "", ""))

	issue, err := store.GetIssue(created.ID)
	require.NoError(t, err)
	require.NoError(t, concreteManager.markAddResultTerminal("owner", issue))

	replayed, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	assert.Equal(t, created.ID, replayed.ID)
	assert.True(t, replayed.Replay)
	assert.True(t, replayed.TerminalReplay)

	for _, listID := range []string{MyListKey, InListKey, OutListKey} {
		references, err := store.GetList("owner", listID)
		require.NoError(t, err)
		for _, reference := range references {
			assert.NotEqual(t, created.ID, reference.IssueID, "list %q retained terminal issue", listID)
		}
	}
	for _, listID := range []string{MyListKey, InListKey, OutListKey} {
		references, err := store.GetList("receiver", listID)
		require.NoError(t, err)
		for _, reference := range references {
			assert.NotEqual(t, receiverIssue.ID, reference.IssueID, "receiver list %q retained terminal issue", listID)
		}
	}
	_, exists, err := store.GetIssueIfExists(created.ID)
	require.NoError(t, err)
	assert.False(t, exists)
	_, exists, err = store.GetIssueIfExists(receiverIssue.ID)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestTerminalReplayRefusesAmbiguousAssignedResidue(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	concreteManager := manager.(*listManager)
	store := NewListStore(api)

	created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	receiverIssue := newIssue(created.Message, created.PostPermalink, created.Description, created.PostID)
	require.NoError(t, store.SaveIssue(receiverIssue))
	require.NoError(t, store.RemoveReference("owner", created.ID, MyListKey))
	require.NoError(t, store.AddReference("owner", created.ID, OutListKey, "receiver", receiverIssue.ID))
	require.NoError(t, store.AddReference("receiver", receiverIssue.ID, InListKey, "different-owner", created.ID))
	require.NoError(t, concreteManager.markAddResultTerminal("owner", created))

	replayed, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	assert.Nil(t, replayed)
	assert.ErrorContains(t, err, "not reciprocal")

	ownerOut, err := store.GetList("owner", OutListKey)
	require.NoError(t, err)
	require.Len(t, ownerOut, 1)
	receiverIn, err := store.GetList("receiver", InListKey)
	require.NoError(t, err)
	require.Len(t, receiverIn, 1)
	_, ownerExists, err := store.GetIssueIfExists(created.ID)
	require.NoError(t, err)
	assert.True(t, ownerExists)
	_, receiverExists, err := store.GetIssueIfExists(receiverIssue.ID)
	require.NoError(t, err)
	assert.True(t, receiverExists)
}

func TestConcurrentDeleteAndReplayNeverRecreatesIssue(t *testing.T) {
	for iteration := range 50 {
		api := newMemoryKVAPI()
		manager := NewListManager(api)
		created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
		require.NoError(t, err)

		start := make(chan struct{})
		removeErr := make(chan error, 1)
		replayResult := make(chan *Issue, 1)
		replayErr := make(chan error, 1)
		go func() {
			<-start
			_, _, _, _, err := manager.RemoveIssue("owner", created.ID)
			removeErr <- err
		}()
		go func() {
			<-start
			issue, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
			replayResult <- issue
			replayErr <- err
		}()
		close(start)

		require.NoError(t, <-removeErr, "iteration %d", iteration)
		require.NoError(t, <-replayErr, "iteration %d", iteration)
		replayed := <-replayResult
		require.NotNil(t, replayed, "iteration %d", iteration)
		assert.Equal(t, created.ID, replayed.ID, "iteration %d", iteration)

		terminalReplay, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
		require.NoError(t, err, "iteration %d", iteration)
		assert.Equal(t, created.ID, terminalReplay.ID, "iteration %d", iteration)
		assert.True(t, terminalReplay.Replay, "iteration %d", iteration)
		assert.True(t, terminalReplay.TerminalReplay, "iteration %d", iteration)

		lists, err := manager.GetAllList("owner")
		require.NoError(t, err, "iteration %d", iteration)
		assert.Empty(t, lists.My, "iteration %d", iteration)
		assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"), "iteration %d", iteration)
		assert.Equal(t, 1, api.countKeysWithPrefix(StoreAddResultKey+"_"), "iteration %d", iteration)

		result, _, err := NewListStore(api).GetAddResult(created.ID)
		require.NoError(t, err, "iteration %d", iteration)
		require.NotNil(t, result, "iteration %d", iteration)
		assert.Equal(t, addResultStateTerminal, result.State, "iteration %d", iteration)
	}
}

func TestAllSelfTodoTerminalActionsPreserveAddTombstone(t *testing.T) {
	testCases := []struct {
		name      string
		terminate func(ListManager, string) error
	}{
		{
			name: "complete",
			terminate: func(manager ListManager, issueID string) error {
				_, _, _, err := manager.CompleteIssue("owner", issueID)
				return err
			},
		},
		{
			name: "pop",
			terminate: func(manager ListManager, _ string) error {
				_, _, err := manager.PopIssue("owner")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			api := newMemoryKVAPI()
			manager := NewListManager(api)
			created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
			require.NoError(t, err)
			require.NoError(t, tc.terminate(manager, created.ID))

			replayed, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
			require.NoError(t, err)
			assert.Equal(t, created.ID, replayed.ID)
			assert.True(t, replayed.Replay)
			assert.True(t, replayed.TerminalReplay)
			assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"))

			result, _, err := NewListStore(api).GetAddResult(created.ID)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, addResultStateTerminal, result.State)
		})
	}
}

func TestConcurrentTerminalActionsHaveExactlyOneWinner(t *testing.T) {
	testCases := []struct {
		name   string
		action func(ListManager, string) error
	}{
		{
			name: "complete",
			action: func(manager ListManager, issueID string) error {
				_, _, _, err := manager.CompleteIssue("owner", issueID)
				return err
			},
		},
		{
			name: "remove",
			action: func(manager ListManager, issueID string) error {
				_, _, _, _, err := manager.RemoveIssue("owner", issueID)
				return err
			},
		},
		{
			name: "pop",
			action: func(manager ListManager, _ string) error {
				_, _, err := manager.PopIssue("owner")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for iteration := range 20 {
				api := newMemoryKVAPI()
				manager := NewListManager(api)
				created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
				require.NoError(t, err)

				const contenders = 16
				start := make(chan struct{})
				results := make(chan error, contenders)
				var wg sync.WaitGroup
				for range contenders {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						results <- tc.action(manager, created.ID)
					}()
				}
				close(start)
				wg.Wait()
				close(results)

				winners := 0
				for result := range results {
					if result == nil {
						winners++
					}
				}
				assert.Equal(t, 1, winners, "iteration %d", iteration)
				assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"), "iteration %d", iteration)

				ownerList, err := NewListStore(api).GetList("owner", MyListKey)
				require.NoError(t, err, "iteration %d", iteration)
				assert.Empty(t, ownerList, "iteration %d", iteration)
				ledger, _, err := NewListStore(api).GetAddResult(created.ID)
				require.NoError(t, err, "iteration %d", iteration)
				require.NotNil(t, ledger, "iteration %d", iteration)
				assert.Equal(t, addResultStateTerminal, ledger.State, "iteration %d", iteration)
			}
		})
	}
}

func TestConcurrentTerminalActionsCollapseDuplicateReferencesIntoOneClaim(t *testing.T) {
	testCases := []struct {
		name   string
		action func(ListManager, string) error
	}{
		{
			name: "complete",
			action: func(manager ListManager, issueID string) error {
				_, _, _, err := manager.CompleteIssue("owner", issueID)
				return err
			},
		},
		{
			name: "remove",
			action: func(manager ListManager, issueID string) error {
				_, _, _, _, err := manager.RemoveIssue("owner", issueID)
				return err
			},
		},
		{
			name: "pop",
			action: func(manager ListManager, _ string) error {
				_, _, err := manager.PopIssue("owner")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for iteration := range 20 {
				api := newMemoryKVAPI()
				manager := NewListManager(api)
				store := NewListStore(api)
				created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
				require.NoError(t, err)

				references, err := store.GetList("owner", MyListKey)
				require.NoError(t, err)
				require.Len(t, references, 1)
				encoded, err := json.Marshal([]*IssueRef{references[0], references[0], references[0]})
				require.NoError(t, err)
				require.Nil(t, api.KVSet(listKey("owner", MyListKey), encoded))

				const contenders = 16
				start := make(chan struct{})
				results := make(chan error, contenders)
				var wg sync.WaitGroup
				for range contenders {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						results <- tc.action(manager, created.ID)
					}()
				}
				close(start)
				wg.Wait()
				close(results)

				winners := 0
				for result := range results {
					if result == nil {
						winners++
					}
				}
				assert.Equal(t, 1, winners, "iteration %d", iteration)
				remaining, err := store.GetList("owner", MyListKey)
				require.NoError(t, err, "iteration %d", iteration)
				assert.Empty(t, remaining, "iteration %d", iteration)
				assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"), "iteration %d", iteration)
				ledger, _, err := store.GetAddResult(created.ID)
				require.NoError(t, err, "iteration %d", iteration)
				require.NotNil(t, ledger, "iteration %d", iteration)
				assert.Equal(t, addResultStateTerminal, ledger.State, "iteration %d", iteration)
			}
		})
	}
}

func TestConcurrentTerminalActionsUseOneClaimAcrossOwnerLists(t *testing.T) {
	testCases := []struct {
		name      string
		extraList string
		action    func(ListManager, string) error
	}{
		{
			name:      "complete-my-and-in",
			extraList: InListKey,
			action: func(manager ListManager, issueID string) error {
				_, _, _, err := manager.CompleteIssue("owner", issueID)
				return err
			},
		},
		{
			name:      "remove-my-and-out",
			extraList: OutListKey,
			action: func(manager ListManager, issueID string) error {
				_, _, _, _, err := manager.RemoveIssue("owner", issueID)
				return err
			},
		},
		{
			name:      "pop-my-and-out",
			extraList: OutListKey,
			action: func(manager ListManager, _ string) error {
				_, _, err := manager.PopIssue("owner")
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for iteration := range 20 {
				api := newMemoryKVAPI()
				manager := NewListManager(api)
				store := NewListStore(api)
				created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
				require.NoError(t, err)
				require.NoError(t, store.AddReference("owner", created.ID, tc.extraList, "", ""))

				const contenders = 16
				start := make(chan struct{})
				results := make(chan error, contenders)
				var wg sync.WaitGroup
				for range contenders {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						results <- tc.action(manager, created.ID)
					}()
				}
				close(start)
				wg.Wait()
				close(results)

				winners := 0
				for result := range results {
					if result == nil {
						winners++
					}
				}
				assert.Equal(t, 1, winners, "iteration %d", iteration)
				for _, listID := range []string{MyListKey, OutListKey, InListKey} {
					remaining, err := store.GetList("owner", listID)
					require.NoError(t, err, "iteration %d list %q", iteration, listID)
					assert.Empty(t, remaining, "iteration %d list %q", iteration, listID)
				}
				assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"), "iteration %d", iteration)
				ledger, _, err := store.GetAddResult(created.ID)
				require.NoError(t, err, "iteration %d", iteration)
				require.NotNil(t, ledger, "iteration %d", iteration)
				assert.Equal(t, addResultStateTerminal, ledger.State, "iteration %d", iteration)
				assert.Equal(t, 1, api.countKeysWithPrefix(StoreTerminalClaimKey+"_"), "iteration %d", iteration)
			}
		})
	}
}

func TestCrossListClaimBlocksTailReaderBetweenListDeletes(t *testing.T) {
	type action func(ListManager, string) error
	complete := func(manager ListManager, issueID string) error {
		_, _, _, err := manager.CompleteIssue("owner", issueID)
		return err
	}
	remove := func(manager ListManager, issueID string) error {
		_, _, _, _, err := manager.RemoveIssue("owner", issueID)
		return err
	}
	pop := func(manager ListManager, _ string) error {
		_, _, err := manager.PopIssue("owner")
		return err
	}
	testCases := []struct {
		name   string
		winner action
		tail   action
	}{
		{name: "complete-then-remove", winner: complete, tail: remove},
		{name: "remove-then-complete", winner: remove, tail: complete},
		{name: "pop-then-remove", winner: pop, tail: remove},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			api := &afterListCASGateAPI{memoryKVAPI: newMemoryKVAPI()}
			manager := NewListManager(api)
			store := NewListStore(api)
			created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
			require.NoError(t, err)
			require.NoError(t, store.AddReference("owner", created.ID, OutListKey, "", ""))

			reached, release := api.enableAfterCASGate(listKey("owner", MyListKey))
			winnerResult := make(chan error, 1)
			go func() { winnerResult <- tc.winner(manager, created.ID) }()

			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("winner did not reach the post-My-CAS gate")
			}

			// My is already absent while Out is still present. This exact read must
			// be stopped by HasCrossListTerminalClaim; deleting that fence makes
			// the tail action a second successful terminal handler.
			tailResult := make(chan error, 1)
			go func() { tailResult <- tc.tail(manager, created.ID) }()
			var tailErr error
			select {
			case tailErr = <-tailResult:
			case <-time.After(2 * time.Second):
				t.Fatal("tail reader did not resolve while winner was gated")
			}
			require.ErrorContains(t, tailErr, "cross-list terminal action already claimed")

			close(release)
			select {
			case err = <-winnerResult:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("winner did not finish after releasing the gate")
			}

			for _, listID := range []string{MyListKey, OutListKey, InListKey} {
				remaining, err := store.GetList("owner", listID)
				require.NoError(t, err)
				assert.Empty(t, remaining)
			}
			assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"))
			assert.Equal(t, 1, api.countKeysWithPrefix(StoreTerminalClaimKey+"_"))
		})
	}
}

func TestConcurrentPopPinsTheOriginalHeadAcrossCASRetries(t *testing.T) {
	api := &gatedListCASAPI{memoryKVAPI: newMemoryKVAPI()}
	manager := NewListManager(api)
	store := NewListStore(api)
	first, err := manager.AddIssue("owner", "first", "", "description", "", "request-first")
	require.NoError(t, err)
	second, err := manager.AddIssue("owner", "second", "", "description", "", "request-second")
	require.NoError(t, err)

	const contenders = 16
	api.enableCASGate(listKey("owner", MyListKey), contenders)
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := manager.PopIssue("owner")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	for result := range results {
		if result == nil {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	remaining, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, second.ID, remaining[0].IssueID)
	_, firstExists, err := store.GetIssueIfExists(first.ID)
	require.NoError(t, err)
	assert.False(t, firstExists)
	_, secondExists, err := store.GetIssueIfExists(second.ID)
	require.NoError(t, err)
	assert.True(t, secondExists)
	firstLedger, _, err := store.GetAddResult(first.ID)
	require.NoError(t, err)
	require.NotNil(t, firstLedger)
	assert.Equal(t, addResultStateTerminal, firstLedger.State)
	secondLedger, _, err := store.GetAddResult(second.ID)
	require.NoError(t, err)
	require.NotNil(t, secondLedger)
	assert.Equal(t, addResultStateCommitted, secondLedger.State)
}

func TestReplayAfterPopClaimBeforeTombstoneDoesNotResurrectIssue(t *testing.T) {
	api := newMemoryKVAPI()
	manager := NewListManager(api)
	store := NewListStore(api)
	created, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)

	// Model a process crash in the only intentional gap: PopReference won its
	// strict CAS claim, but the terminal ledger transition has not run yet.
	claimed, err := store.PopReference("owner", MyListKey, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, claimed.IssueID)
	ledger, _, err := store.GetAddResult(created.ID)
	require.NoError(t, err)
	assert.Equal(t, addResultStateCommitted, ledger.State)

	replayed, err := manager.AddIssue("owner", "title", "", "description", "", "request-1")
	require.NoError(t, err)
	assert.Equal(t, created.ID, replayed.ID)
	assert.True(t, replayed.Replay)
	assert.True(t, replayed.TerminalReplay)
	assert.Equal(t, 0, api.countKeysWithPrefix(StoreIssueKey+"_"))
	ownerList, err := store.GetList("owner", MyListKey)
	require.NoError(t, err)
	assert.Empty(t, ownerList)
	ledger, _, err = store.GetAddResult(created.ID)
	require.NoError(t, err)
	assert.Equal(t, addResultStateTerminal, ledger.State)
}

func TestAddIssueClientRequestIDRejectsPayloadMismatch(t *testing.T) {
	manager := NewListManager(newMemoryKVAPI())

	original, err := manager.AddIssue("owner", "original", "", "description", "", "request-1")
	require.NoError(t, err)

	conflict, err := manager.AddIssue("owner", "changed", "", "description", "", "request-1")
	assert.Nil(t, conflict)
	assert.True(t, errors.Is(err, errClientRequestIDConflict))

	lists, listErr := manager.GetAllList("owner")
	require.NoError(t, listErr)
	require.Len(t, lists.My, 1)
	assert.Equal(t, original.ID, lists.My[0].ID)
	assert.Equal(t, "original", lists.My[0].Message)
}

func TestAddIssueClientRequestIDIsScopedByActor(t *testing.T) {
	manager := NewListManager(newMemoryKVAPI())

	first, err := manager.AddIssue("owner-a", "title", "", "", "", "same-request")
	require.NoError(t, err)
	second, err := manager.AddIssue("owner-b", "title", "", "", "", "same-request")
	require.NoError(t, err)

	assert.NotEqual(t, first.ID, second.ID)
}

func TestAddIssueWithoutClientRequestIDPreservesLegacyNonIdempotentBehavior(t *testing.T) {
	manager := NewListManager(newMemoryKVAPI())

	first, err := manager.AddIssue("owner", "title", "", "", "", "")
	require.NoError(t, err)
	second, err := manager.AddIssue("owner", "title", "", "", "", "")
	require.NoError(t, err)

	assert.NotEqual(t, first.ID, second.ID)
	lists, err := manager.GetAllList("owner")
	require.NoError(t, err)
	assert.Len(t, lists.My, 2)
}

type addListManagerStub struct {
	ListManager

	issue *Issue
	err   error
	calls int
}

type changeAssignmentListManagerStub struct {
	ListManager

	err   error
	calls int
}

func (s *changeAssignmentListManagerStub) ChangeAssignment(_, _, _ string) (*Issue, string, string, error) {
	s.calls++
	return nil, "", "", s.err
}

func (s *addListManagerStub) AddIssue(_, _, _, _, _, clientRequestID string) (*Issue, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	issue := *s.issue
	issue.ClientRequestID = clientRequestID
	return &issue, nil
}

func TestHandleAddMapsClientRequestPayloadMismatchToConflict(t *testing.T) {
	api := &plugintest.API{}
	manager := &addListManagerStub{err: errClientRequestIDConflict}
	p := &Plugin{listManager: manager, tracker: noopTelemetryTracker{}}
	p.SetAPI(api)
	api.On("LogError", ErrorMsgAddIssue, "err", errClientRequestIDConflict.Error()).Return().Once()

	request := httptest.NewRequest(
		http.MethodPost,
		"/add",
		strings.NewReader(`{"message":"changed","client_request_id":"request-1"}`),
	)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()

	p.handleAdd(response, request)

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.JSONEq(t, `{
		"error":"Unable to add issue",
		"details":"client_request_id is already bound to a different add payload"
	}`, response.Body.String())
	assert.Equal(t, 1, manager.calls)
	api.AssertExpectations(t)
}

func TestHandleChangeAssignmentMapsCorrelatedIssueGuardToConflict(t *testing.T) {
	api := &plugintest.API{}
	manager := &changeAssignmentListManagerStub{err: errCorrelatedIssueAssignmentUnsupported}
	p := &Plugin{listManager: manager, tracker: noopTelemetryTracker{}}
	p.SetAPI(api)
	api.On("GetUserByUsername", "receiver").Return(
		&model.User{Id: "receiver-id", Username: "receiver"},
		(*model.AppError)(nil),
	).Once()
	api.On(
		"LogError",
		"Unable to change the assignment of an issue",
		"err",
		errCorrelatedIssueAssignmentUnsupported.Error(),
	).Return().Once()

	request := httptest.NewRequest(
		http.MethodPost,
		"/change_assignment",
		strings.NewReader(`{"id":"correlated-issue","send_to":"receiver"}`),
	)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()

	p.handleChangeAssignment(response, request)

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.JSONEq(t, `{
		"error":"Unable to change the assignment of an issue",
		"details":"assignment is unavailable for a client_request_id todo"
	}`, response.Body.String())
	assert.Equal(t, 1, manager.calls)
	api.AssertExpectations(t)
}

func TestHandleAddRejectsClientRequestIDForSendTo(t *testing.T) {
	manager := &addListManagerStub{issue: &Issue{ID: "unused"}}
	p := &Plugin{listManager: manager}
	request := httptest.NewRequest(
		http.MethodPost,
		"/add",
		strings.NewReader(`{"message":"title","send_to":"receiver","client_request_id":"request-1"}`),
	)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()

	p.handleAdd(response, request)

	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "client_request_id is supported only for a self todo")
	assert.Zero(t, manager.calls)
}

func (s *addListManagerStub) GetUserName(string) string {
	return "actor"
}

func TestHandleAddReturnsExactIdentity(t *testing.T) {
	api := &plugintest.API{}
	manager := &addListManagerStub{issue: &Issue{ID: "mattermost-issue-id"}}
	p := &Plugin{listManager: manager, tracker: noopTelemetryTracker{}}
	p.SetAPI(api)

	api.On(
		"PublishWebSocketEvent",
		WSEventRefresh,
		map[string]interface{}{"lists": []string{MyListKey}},
		mock.MatchedBy(func(broadcast *model.WebsocketBroadcast) bool {
			return broadcast != nil && broadcast.UserId == "owner"
		}),
	).Return().Once()

	request := httptest.NewRequest(
		http.MethodPost,
		"/add",
		strings.NewReader(`{"message":"title","description":"description","client_request_id":"request-1"}`),
	)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()

	p.handleAdd(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{"id":"mattermost-issue-id","client_request_id":"request-1","terminal":false}`, response.Body.String())
	assert.Equal(t, 1, manager.calls)
	api.AssertExpectations(t)
}

func TestHandleAddTerminalReplayReturnsExactIdentityWithoutSideEffects(t *testing.T) {
	api := &plugintest.API{}
	manager := &addListManagerStub{issue: &Issue{ID: "mattermost-issue-id", Replay: true, TerminalReplay: true}}
	p := &Plugin{listManager: manager, tracker: noopTelemetryTracker{}}
	p.SetAPI(api)

	request := httptest.NewRequest(
		http.MethodPost,
		"/add",
		strings.NewReader(`{"message":"title","description":"description","client_request_id":"request-1"}`),
	)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()

	p.handleAdd(response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{"id":"mattermost-issue-id","client_request_id":"request-1","terminal":true}`, response.Body.String())
	api.AssertNotCalled(t, "PublishWebSocketEvent", mock.Anything, mock.Anything, mock.Anything)
}

func TestAddRequestClientRequestIDValidation(t *testing.T) {
	testCases := []struct {
		name string
		id   string
	}{
		{name: "leading whitespace", id: " request-1"},
		{name: "trailing whitespace", id: "request-1 "},
		{name: "control character", id: "request\n1"},
		{name: "over max rune length", id: strings.Repeat("я", maxClientRequestIDLength+1)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			request := &AddAPIRequest{Message: "title", ClientRequestID: tc.id}
			assert.Error(t, request.IsValid())
		})
	}

	assert.NoError(t, (&AddAPIRequest{
		Message:         "title",
		ClientRequestID: strings.Repeat("я", maxClientRequestIDLength),
	}).IsValid())
}

func TestCapabilitiesHandshakeIsAuthenticatedAndExact(t *testing.T) {
	p := &Plugin{}
	p.initializeAPI()

	unauthorized := httptest.NewRecorder()
	p.ServeHTTP(nil, unauthorized, httptest.NewRequest(http.MethodGet, "/capabilities", nil))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	request := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	request.Header.Set("Mattermost-User-ID", "owner")
	response := httptest.NewRecorder()
	p.ServeHTTP(nil, response, request)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{
		"protocol_version": 1,
		"features": {
			"add_client_request_id": true,
			"add_response_identity": true,
			"add_idempotent": true,
			"add_terminal_replay": true,
			"list_client_request_id": true
		}
	}`, response.Body.String())
}
