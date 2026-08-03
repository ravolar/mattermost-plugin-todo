package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

const (
	// StoreRetries is the number of retries to use when storing lists fails on a race
	StoreRetries = 3
	// StoreListKey is the key used to store lists in the plugin KV store. Still "order" for backwards compatibility.
	StoreListKey = "order"
	// StoreIssueKey is the key used to store issues in the plugin KV store. Still "item" for backwards compatibility.
	StoreIssueKey = "item"
	// StoreAddResultKey stores durable add results independently from issues.
	// There is intentionally no delete path: a future bounded-retention policy
	// must preserve the no-recreation guarantee or prove a maximum replay horizon.
	StoreAddResultKey = "add_result"
	// StoreTerminalClaimKey is a permanent actor+issue claim used only to
	// collapse abnormal cross-list residue into one terminal action. Normal
	// single-list actions keep their existing list CAS claim.
	StoreTerminalClaimKey = "terminal_claim"
	// StoreReminderKey is the key used to store the last time a user was reminded
	StoreReminderKey = "reminder"
	// StoreReminderEnabledKey is the key used to store the user preference of auto daily reminder
	StoreReminderEnabledKey = "reminder_enabled"

	// StoreAllowIncomingTaskRequestsKey is the key used to store user preference for wallowing any incoming todo requests
	StoreAllowIncomingTaskRequestsKey = "allow_incoming_task"
)

// IssueRef denotes every element in any of the lists. Contains the issue that refers to,
// and may contain foreign ids of issue and user, denoting the user this element is related to
// and the issue on that user system.
type IssueRef struct {
	IssueID        string `json:"issue_id"`
	ForeignIssueID string `json:"foreign_issue_id"`
	ForeignUserID  string `json:"foreign_user_id"`
}

type addResultLedger struct {
	Version         int    `json:"version"`
	State           string `json:"state"`
	UserID          string `json:"user_id"`
	ClientRequestID string `json:"client_request_id"`
	IssueID         string `json:"issue_id"`
	PayloadSHA256   string `json:"payload_sha256"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	TerminalAt      int64  `json:"terminal_at,omitempty"`
}

func listKey(userID string, listID string) string {
	return fmt.Sprintf("%s_%s%s", StoreListKey, userID, listID)
}

func issueKey(issueID string) string {
	return fmt.Sprintf("%s_%s", StoreIssueKey, issueID)
}

func addResultKey(issueID string) string {
	return fmt.Sprintf("%s_%s", StoreAddResultKey, issueID)
}

func terminalClaimKey(userID, issueID string) string {
	// Length-prefix the actor so two variable-length IDs cannot alias.
	return fmt.Sprintf("%s_%d:%s:%s", StoreTerminalClaimKey, len(userID), userID, issueID)
}

func reminderKey(userID string) string {
	return fmt.Sprintf("%s_%s", StoreReminderKey, userID)
}

func reminderEnabledKey(userID string) string {
	return fmt.Sprintf("%s_%s", StoreReminderEnabledKey, userID)
}

func allowIncomingTaskRequestsKey(userID string) string {
	return fmt.Sprintf("%s_%s", StoreAllowIncomingTaskRequestsKey, userID)
}

type listStore struct {
	api plugin.API
}

func (l *listStore) ClaimCrossListTerminalAction(userID, issueID string) (bool, error) {
	value, err := json.Marshal(struct {
		Version int    `json:"version"`
		UserID  string `json:"user_id"`
		IssueID string `json:"issue_id"`
	}{Version: 1, UserID: userID, IssueID: issueID})
	if err != nil {
		return false, err
	}
	ok, appErr := l.api.KVCompareAndSet(terminalClaimKey(userID, issueID), nil, value)
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}
	return ok, nil
}

func (l *listStore) HasCrossListTerminalClaim(userID, issueID string) (bool, error) {
	value, appErr := l.api.KVGet(terminalClaimKey(userID, issueID))
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}
	return value != nil, nil
}

// NewListStore creates a new listStore
func NewListStore(api plugin.API) ListStore {
	return &listStore{
		api: api,
	}
}

func (l *listStore) SaveIssue(issue *Issue) error {
	jsonIssue, jsonErr := json.Marshal(issue)
	if jsonErr != nil {
		return jsonErr
	}

	appErr := l.api.KVSet(issueKey(issue.ID), jsonIssue)
	if appErr != nil {
		return errors.New(appErr.Error())
	}

	return nil
}

func (l *listStore) SaveIssueIfAbsent(issue *Issue) (bool, error) {
	jsonIssue, jsonErr := json.Marshal(issue)
	if jsonErr != nil {
		return false, jsonErr
	}

	ok, appErr := l.api.KVCompareAndSet(issueKey(issue.ID), nil, jsonIssue)
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}

	return ok, nil
}

func (l *listStore) SaveAddResultIfAbsent(result *addResultLedger) (bool, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return false, err
	}

	ok, appErr := l.api.KVCompareAndSet(addResultKey(result.IssueID), nil, encoded)
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}
	return ok, nil
}

func (l *listStore) GetAddResult(issueID string) (*addResultLedger, []byte, error) {
	original, appErr := l.api.KVGet(addResultKey(issueID))
	if appErr != nil {
		return nil, nil, errors.New(appErr.Error())
	}
	if original == nil {
		return nil, nil, nil
	}

	var result addResultLedger
	if err := json.Unmarshal(original, &result); err != nil {
		return nil, nil, err
	}
	return &result, original, nil
}

func (l *listStore) CompareAndSetAddResult(original []byte, result *addResultLedger) (bool, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return false, err
	}

	ok, appErr := l.api.KVCompareAndSet(addResultKey(result.IssueID), original, encoded)
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}
	return ok, nil
}

func (l *listStore) GetIssue(issueID string) (*Issue, error) {
	issue, exists, err := l.GetIssueIfExists(issueID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("cannot find issue")
	}
	return issue, nil
}

func (l *listStore) GetIssueIfExists(issueID string) (*Issue, bool, error) {
	originalJSONIssue, appErr := l.api.KVGet(issueKey(issueID))
	if appErr != nil {
		return nil, false, errors.New(appErr.Error())
	}

	if originalJSONIssue == nil {
		return nil, false, nil
	}

	var issue *Issue
	err := json.Unmarshal(originalJSONIssue, &issue)
	if err != nil {
		return nil, false, err
	}

	return issue, true, nil
}

func (l *listStore) RemoveIssue(issueID string) error {
	appErr := l.api.KVDelete(issueKey(issueID))
	if appErr != nil {
		return errors.New(appErr.Error())
	}

	return nil
}

func (l *listStore) GetAndRemoveIssue(issueID string) (*Issue, error) {
	issue, err := l.GetIssue(issueID)
	if err != nil {
		return nil, err
	}

	err = l.RemoveIssue(issueID)
	if err != nil {
		return nil, err
	}

	return issue, nil
}

func (l *listStore) GetIssueReference(userID, issueID, listID string) (*IssueRef, int, error) {
	originalJSONList, err := l.api.KVGet(listKey(userID, listID))
	if err != nil {
		return nil, 0, err
	}

	if originalJSONList == nil {
		return nil, 0, errors.New("cannot load list")
	}

	var list []*IssueRef
	jsonErr := json.Unmarshal(originalJSONList, &list)
	if jsonErr != nil {
		list, _, jsonErr = l.legacyIssueRef(userID, listID)
		if list == nil {
			return nil, 0, jsonErr
		}
	}

	for i, ir := range list {
		if ir.IssueID == issueID {
			return ir, i, nil
		}
	}
	return nil, 0, errors.New("cannot find issue")
}

func (l *listStore) GetIssueListAndReference(userID, issueID string) (string, *IssueRef, int) {
	ir, n, _ := l.GetIssueReference(userID, issueID, MyListKey)
	if ir != nil {
		return MyListKey, ir, n
	}

	ir, n, _ = l.GetIssueReference(userID, issueID, OutListKey)
	if ir != nil {
		return OutListKey, ir, n
	}

	ir, n, _ = l.GetIssueReference(userID, issueID, InListKey)
	if ir != nil {
		return InListKey, ir, n
	}

	return "", nil, 0
}

func (l *listStore) AddReference(userID, issueID, listID, foreignUserID, foreignIssueID string) error {
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return err
		}

		for _, ir := range list {
			if ir.IssueID == issueID {
				return errors.New("issue id already exists in list")
			}
		}

		list = append(list, &IssueRef{
			IssueID:        issueID,
			ForeignIssueID: foreignIssueID,
			ForeignUserID:  foreignUserID,
		})

		ok, err := l.saveList(userID, listID, list, originalJSONList)
		if err != nil {
			return err
		}

		// If err is nil but ok is false, then something else updated the installs between the get and set above
		// so we need to try again, otherwise we can return
		if ok {
			return nil
		}
	}

	return errors.New("unable to store installation")
}

func (l *listStore) EnsureReference(userID, issueID, listID, foreignUserID, foreignIssueID string) error {
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return err
		}

		for _, ir := range list {
			if ir.IssueID != issueID {
				continue
			}
			if ir.ForeignUserID == foreignUserID && ir.ForeignIssueID == foreignIssueID {
				return nil
			}
			return errors.New("issue id already exists with different reference metadata")
		}

		list = append(list, &IssueRef{
			IssueID:        issueID,
			ForeignIssueID: foreignIssueID,
			ForeignUserID:  foreignUserID,
		})

		ok, err := l.saveList(userID, listID, list, originalJSONList)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}

	return errors.New("unable to ensure issue reference")
}

func (l *listStore) RemoveReferenceIfExists(userID, issueID, listID string) error {
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return err
		}

		found := false
		filtered := make([]*IssueRef, 0, len(list))
		for _, ir := range list {
			if ir.IssueID == issueID {
				found = true
				continue
			}
			filtered = append(filtered, ir)
		}
		if !found {
			return nil
		}
		list = filtered

		ok, err := l.saveList(userID, listID, list, originalJSONList)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}

	return errors.New("unable to remove issue reference")
}

func (l *listStore) RemoveReference(userID, issueID, listID string) error {
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return err
		}

		// The strict removal is also the terminal-action claim. Filter every
		// duplicate in one CAS write: mutating list while ranging can skip entries
		// (or slice past the shortened length), while leaving a duplicate would let
		// a second concurrent terminal action claim the same physical issue.
		found := false
		filtered := make([]*IssueRef, 0, len(list))
		for _, ir := range list {
			if ir.IssueID == issueID {
				found = true
				continue
			}
			filtered = append(filtered, ir)
		}

		if !found {
			return errors.New("cannot find issue")
		}

		ok, err := l.saveList(userID, listID, filtered, originalJSONList)
		if err != nil {
			return err
		}

		// If err is nil but ok is false, then something else updated the installs between the get and set above
		// so we need to try again, otherwise we can return
		if ok {
			return nil
		}
	}

	return errors.New("unable to store list")
}

func (l *listStore) PopReference(userID, listID, expectedIssueID string) (*IssueRef, error) {
	var claimed *IssueRef
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return nil, err
		}

		if len(list) == 0 {
			return nil, errors.New("cannot find issue")
		}

		if claimed == nil {
			if expectedIssueID != "" && list[0].IssueID != expectedIssueID {
				return nil, errors.New("list head changed while claiming issue")
			}
			copy := *list[0]
			claimed = &copy
		} else if list[0].IssueID != claimed.IssueID ||
			list[0].ForeignUserID != claimed.ForeignUserID ||
			list[0].ForeignIssueID != claimed.ForeignIssueID {
			// A lost CAS may expose the next item. This call claimed only the
			// original head and must not silently pop a different task on retry.
			return nil, errors.New("list head changed while claiming issue")
		}

		// Treat exact-ID duplicates of the head as one physical claim. Removing
		// only list[0] would allow concurrent pop calls to both succeed for it.
		filtered := make([]*IssueRef, 0, len(list)-1)
		for _, candidate := range list[1:] {
			if candidate.IssueID != claimed.IssueID {
				filtered = append(filtered, candidate)
			}
		}

		ok, err := l.saveList(userID, listID, filtered, originalJSONList)
		if err != nil {
			return nil, err
		}

		// If err is nil but ok is false, then something else updated the installs between the get and set above
		// so we need to try again, otherwise we can return
		if ok {
			return claimed, nil
		}
	}

	return nil, errors.New("unable to store list")
}

func (l *listStore) BumpReference(userID, issueID, listID string) error {
	for i := 0; i < StoreRetries; i++ {
		list, originalJSONList, err := l.getList(userID, listID)
		if err != nil {
			return err
		}

		var i int
		var ir *IssueRef

		for i, ir = range list {
			if issueID == ir.IssueID {
				break
			}
		}

		if i == len(list) {
			return errors.New("cannot find issue")
		}

		newList := append([]*IssueRef{ir}, list[:i]...)
		newList = append(newList, list[i+1:]...)

		ok, err := l.saveList(userID, listID, newList, originalJSONList)
		if err != nil {
			return err
		}

		// If err is nil but ok is false, then something else updated the installs between the get and set above
		// so we need to try again, otherwise we can return
		if ok {
			return nil
		}
	}

	return errors.New("unable to store list")
}

func (l *listStore) GetList(userID, listID string) ([]*IssueRef, error) {
	irs, _, err := l.getList(userID, listID)
	return irs, err
}

func (l *listStore) getList(userID, listID string) ([]*IssueRef, []byte, error) {
	originalJSONList, err := l.api.KVGet(listKey(userID, listID))
	if err != nil {
		return nil, nil, err
	}

	if originalJSONList == nil {
		return []*IssueRef{}, originalJSONList, nil
	}

	var list []*IssueRef
	jsonErr := json.Unmarshal(originalJSONList, &list)
	if jsonErr != nil {
		return l.legacyIssueRef(userID, listID)
	}

	return list, originalJSONList, nil
}

func (l *listStore) saveList(userID, listID string, list []*IssueRef, originalJSONList []byte) (bool, error) {
	newJSONList, jsonErr := json.Marshal(list)
	if jsonErr != nil {
		return false, jsonErr
	}

	ok, appErr := l.api.KVCompareAndSet(listKey(userID, listID), originalJSONList, newJSONList)
	if appErr != nil {
		return false, errors.New(appErr.Error())
	}

	return ok, nil
}

func (l *listStore) legacyIssueRef(userID, listID string) ([]*IssueRef, []byte, error) {
	originalJSONList, err := l.api.KVGet(listKey(userID, listID))
	if err != nil {
		return nil, nil, err
	}

	if originalJSONList == nil {
		return []*IssueRef{}, originalJSONList, nil
	}

	var list []string
	jsonErr := json.Unmarshal(originalJSONList, &list)
	if jsonErr != nil {
		return nil, nil, jsonErr
	}

	newList := []*IssueRef{}
	for _, v := range list {
		newList = append(newList, &IssueRef{IssueID: v})
	}

	return newList, originalJSONList, nil
}

func (p *Plugin) saveLastReminderTimeForUser(userID string) error {
	strTime := strconv.FormatInt(model.GetMillis(), 10)
	appErr := p.API.KVSet(reminderKey(userID), []byte(strTime))
	if appErr != nil {
		return errors.New(appErr.Error())
	}
	return nil
}

func (p *Plugin) getLastReminderTimeForUser(userID string) (int64, error) {
	timeBytes, appErr := p.API.KVGet(reminderKey(userID))
	if appErr != nil {
		return 0, errors.New(appErr.Error())
	}

	if timeBytes == nil {
		return 0, nil
	}

	reminderAt, err := strconv.ParseInt(string(timeBytes), 10, 64)
	if err != nil {
		return 0, err
	}

	return reminderAt, nil
}

func (p *Plugin) saveReminderPreference(userID string, preference bool) error {
	preferenceString := strconv.FormatBool(preference)
	appErr := p.API.KVSet(reminderEnabledKey(userID), []byte(preferenceString))
	if appErr != nil {
		return appErr
	}
	return nil
}

// getReminderPreference - gets user preference on reminder - default value will be true if in case any error
func (p *Plugin) getReminderPreference(userID string) bool {
	preferenceByte, appErr := p.API.KVGet(reminderEnabledKey(userID))
	if appErr != nil {
		p.API.LogError("error getting the reminder preference, err=", appErr.Error())
		return true
	}

	if preferenceByte == nil {
		p.API.LogInfo(`reminder preference is empty. Defaulting to "on"`)
		return true
	}

	preference, err := strconv.ParseBool(string(preferenceByte))
	if err != nil {
		p.API.LogError("unable to parse the reminder preference, err=", err.Error())
		return true
	}

	return preference
}

func (p *Plugin) saveAllowIncomingTaskRequestsPreference(userID string, preference bool) error {
	preferenceString := strconv.FormatBool(preference)
	appErr := p.API.KVSet(allowIncomingTaskRequestsKey(userID), []byte(preferenceString))
	if appErr != nil {
		return appErr
	}
	return nil
}

// getAllowIncomingTaskRequestsPreference - gets user preference on allowing incoming task requests from other users - default value will be true if in case any error
func (p *Plugin) getAllowIncomingTaskRequestsPreference(userID string) (bool, error) {
	preferenceByte, appErr := p.API.KVGet(allowIncomingTaskRequestsKey(userID))
	if appErr != nil {
		err := errors.Wrap(appErr, "error getting the allow incoming task requests preference")
		return true, err
	}

	if preferenceByte == nil {
		p.API.LogDebug(`allow incoming task requests is empty. Defaulting to "on"`)
		return true, nil
	}

	preference, err := strconv.ParseBool(string(preferenceByte))
	if err != nil {
		err := errors.Wrap(appErr, "unable to parse the allow incoming task requests preference")
		return true, err
	}

	return preference, nil
}
