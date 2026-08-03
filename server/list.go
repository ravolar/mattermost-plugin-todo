package main

import (
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

const (
	// MyListKey is the key used to store the list of the owned todos
	MyListKey = ""
	// InListKey is the key used to store the list of received todos
	InListKey = "_in"
	// OutListKey is the key used to store the list of sent todos
	OutListKey = "_out"
)

var (
	errClientRequestIDConflict              = errors.New("client_request_id is already bound to a different add payload")
	errCorrelatedIssueAssignmentUnsupported = errors.New("assignment is unavailable for a client_request_id todo")
)

const (
	addResultVersion        = 1
	addResultStateReserved  = "reserved"
	addResultStateCommitted = "committed"
	addResultStateTerminal  = "terminal"
)

// ListStore represents the KVStore operations for lists
type ListStore interface {
	// Issue related function
	SaveIssue(issue *Issue) error
	SaveIssueIfAbsent(issue *Issue) (bool, error)
	GetIssueIfExists(issueID string) (*Issue, bool, error)
	GetIssue(issueID string) (*Issue, error)
	RemoveIssue(issueID string) error
	GetAndRemoveIssue(issueID string) (*Issue, error)
	SaveAddResultIfAbsent(result *addResultLedger) (bool, error)
	GetAddResult(issueID string) (*addResultLedger, []byte, error)
	CompareAndSetAddResult(original []byte, result *addResultLedger) (bool, error)
	ClaimCrossListTerminalAction(userID, issueID string) (bool, error)
	HasCrossListTerminalClaim(userID, issueID string) (bool, error)

	// Issue References related functions

	// AddReference creates a new IssueRef with the issueID, foreignUSerID and foreignIssueID, and stores it
	// on the listID for userID.
	AddReference(userID, issueID, listID, foreignUserID, foreignIssueID string) error
	// EnsureReference is the idempotent equivalent of AddReference. An exact
	// existing reference is success; conflicting reference metadata is an error.
	EnsureReference(userID, issueID, listID, foreignUserID, foreignIssueID string) error
	RemoveReferenceIfExists(userID, issueID, listID string) error
	// RemoveReference removes the IssueRef for issueID in listID for userID
	RemoveReference(userID, issueID, listID string) error
	// PopReference removes the expected first IssueRef in listID for userID and
	// returns it. expectedIssueID pins an earlier manager-level head read.
	PopReference(userID, listID, expectedIssueID string) (*IssueRef, error)
	// BumpReference moves the Issue reference for issueID in listID for userID to the beginning of the list
	BumpReference(userID, issueID, listID string) error
	// GetIssueReference gets the IssueRef and position of the issue issueID on user userID's list listID
	GetIssueReference(userID, issueID, listID string) (*IssueRef, int, error)
	// GetIssueListAndReference gets the issue list, IssueRef and position for user userID
	GetIssueListAndReference(userID, issueID string) (string, *IssueRef, int)
	// GetList returns the list of IssueRef in listID for userID
	GetList(userID, listID string) ([]*IssueRef, error)
}

type listManager struct {
	store ListStore
	api   plugin.API
}

type terminalReferenceClaim struct {
	listID    string
	reference *IssueRef
	crossList bool
}

func (l *listManager) resolveTerminalReferenceClaim(userID, issueID string) (*terminalReferenceClaim, error) {
	var selected *terminalReferenceClaim
	foundLists := 0
	for _, listID := range []string{MyListKey, OutListKey, InListKey} {
		references, err := l.store.GetList(userID, listID)
		if err != nil {
			return nil, err
		}
		var inList *IssueRef
		for _, reference := range references {
			if reference.IssueID != issueID {
				continue
			}
			if inList != nil && (inList.ForeignUserID != reference.ForeignUserID ||
				inList.ForeignIssueID != reference.ForeignIssueID) {
				return nil, errors.New("conflicting terminal references in one list")
			}
			inList = reference
		}
		if inList == nil {
			continue
		}
		foundLists++
		if selected != nil && (selected.reference.ForeignUserID != inList.ForeignUserID ||
			selected.reference.ForeignIssueID != inList.ForeignIssueID) {
			return nil, errors.New("conflicting terminal references across lists")
		}
		if selected == nil {
			selected = &terminalReferenceClaim{listID: listID, reference: inList}
		}
	}
	if selected == nil {
		return nil, errors.New("cannot find element")
	}

	if foundLists > 1 {
		claimed, err := l.store.ClaimCrossListTerminalAction(userID, issueID)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return nil, errors.New("cross-list terminal action already claimed")
		}
		selected.crossList = true
		return selected, nil
	}

	// A cross-list winner may already be collapsing residue while this reader
	// observed only its last remaining list. Never let that tail become a
	// second independent list-level claim.
	claimed, err := l.store.HasCrossListTerminalClaim(userID, issueID)
	if err != nil {
		return nil, err
	}
	if claimed {
		return nil, errors.New("cross-list terminal action already claimed")
	}
	return selected, nil
}

func (l *listManager) consumeTerminalReferenceClaim(userID, issueID string, claim *terminalReferenceClaim) error {
	if !claim.crossList {
		return l.store.RemoveReference(userID, issueID, claim.listID)
	}
	for _, listID := range []string{MyListKey, OutListKey, InListKey} {
		if err := l.store.RemoveReferenceIfExists(userID, issueID, listID); err != nil {
			return err
		}
	}
	return nil
}

// NewListManager creates a new listManager
func NewListManager(api plugin.API) ListManager {
	return &listManager{
		store: NewListStore(api),
		api:   api,
	}
}

func (l *listManager) AddIssue(userID, message, postPermalink, description, postID, clientRequestID string) (*Issue, error) {
	if clientRequestID != "" {
		return l.addIssueIdempotent(userID, message, postPermalink, description, postID, clientRequestID)
	}

	issue := newIssue(message, postPermalink, description, postID)

	if err := l.store.SaveIssue(issue); err != nil {
		return nil, err
	}

	if err := l.store.AddReference(userID, issue.ID, MyListKey, "", ""); err != nil {
		if rollbackError := l.store.RemoveIssue(issue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback issue after add error, Err=", err.Error())
		}
		return nil, err
	}

	return issue, nil
}

func (l *listManager) addIssueIdempotent(userID, message, postPermalink, description, postID, clientRequestID string) (*Issue, error) {
	candidate := newIdempotentIssue(userID, message, postPermalink, description, postID, clientRequestID)
	payloadSHA256 := addPayloadFingerprint(message, postPermalink, description, postID)
	ledgerCandidate := &addResultLedger{
		Version:         addResultVersion,
		State:           addResultStateReserved,
		UserID:          userID,
		ClientRequestID: clientRequestID,
		IssueID:         candidate.ID,
		PayloadSHA256:   payloadSHA256,
		CreatedAt:       candidate.CreateAt,
		UpdatedAt:       candidate.CreateAt,
	}
	_, err := l.store.SaveAddResultIfAbsent(ledgerCandidate)
	if err != nil {
		return nil, err
	}

	ledger, original, err := l.store.GetAddResult(candidate.ID)
	if err != nil {
		return nil, err
	}
	if err := validateAddResult(ledger, candidate, userID, clientRequestID, payloadSHA256); err != nil {
		return nil, err
	}

	switch ledger.State {
	case addResultStateTerminal:
		if err := l.cleanupTerminalAdd(userID, candidate.ID); err != nil {
			return nil, err
		}
		return replayIssueFromLedger(ledger), nil
	case addResultStateCommitted:
		return l.replayCommittedAdd(ledger, candidate)
	case addResultStateReserved:
		// Continue below. Any caller holding the same immutable request can
		// safely help finish a reservation left by a crash or concurrent call.
	default:
		return nil, errors.Errorf("invalid add result state %q", ledger.State)
	}

	createdIssue, err := l.store.SaveIssueIfAbsent(candidate)
	if err != nil {
		return nil, err
	}
	issue := candidate
	if !createdIssue {
		var exists bool
		issue, exists, err = l.store.GetIssueIfExists(candidate.ID)
		if err != nil {
			return nil, err
		}
		if !exists || !issueMatchesAddIdentity(issue, ledger) {
			return nil, errClientRequestIDConflict
		}
	}

	if err := l.store.EnsureReference(userID, issue.ID, MyListKey, "", ""); err != nil {
		return nil, err
	}

	for i := 0; i < StoreRetries; i++ {
		committed := *ledger
		committed.State = addResultStateCommitted
		committed.UpdatedAt = model.GetMillis()
		ok, err := l.store.CompareAndSetAddResult(original, &committed)
		if err != nil {
			return nil, err
		}
		if ok {
			issue.Replay = !createdIssue
			return issue, nil
		}

		ledger, original, err = l.store.GetAddResult(candidate.ID)
		if err != nil {
			return nil, err
		}
		if err := validateAddResult(ledger, candidate, userID, clientRequestID, payloadSHA256); err != nil {
			return nil, err
		}
		switch ledger.State {
		case addResultStateCommitted:
			issue.Replay = !createdIssue
			return issue, nil
		case addResultStateTerminal:
			if err := l.cleanupTerminalAdd(userID, candidate.ID); err != nil {
				return nil, err
			}
			return replayIssueFromLedger(ledger), nil
		case addResultStateReserved:
			continue
		default:
			return nil, errors.Errorf("invalid add result state %q", ledger.State)
		}
	}

	return nil, errors.New("unable to commit durable add result")
}

func validateAddResult(result *addResultLedger, candidate *Issue, userID, clientRequestID, payloadSHA256 string) error {
	if result == nil ||
		result.Version != addResultVersion ||
		result.UserID != userID ||
		result.ClientRequestID != clientRequestID ||
		result.IssueID != candidate.ID ||
		result.PayloadSHA256 != payloadSHA256 {
		return errClientRequestIDConflict
	}
	return nil
}

func issueMatchesAddIdentity(issue *Issue, result *addResultLedger) bool {
	return issue != nil &&
		issue.ID == result.IssueID &&
		issue.ClientRequestID == result.ClientRequestID
}

func replayIssueFromLedger(result *addResultLedger) *Issue {
	return &Issue{
		ID:              result.IssueID,
		ClientRequestID: result.ClientRequestID,
		CreateAt:        result.CreatedAt,
		Replay:          true,
		TerminalReplay:  true,
	}
}

func (l *listManager) replayCommittedAdd(result *addResultLedger, candidate *Issue) (*Issue, error) {
	issue, exists, err := l.store.GetIssueIfExists(result.IssueID)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := l.markAddResultTerminal(result.UserID, candidate); err != nil {
			return nil, err
		}
		if err := l.cleanupTerminalAdd(result.UserID, result.IssueID); err != nil {
			return nil, err
		}
		return replayIssueFromLedger(result), nil
	}
	if !issueMatchesAddIdentity(issue, result) {
		return nil, errClientRequestIDConflict
	}

	found := false
	for _, listID := range []string{MyListKey, InListKey, OutListKey} {
		references, err := l.store.GetList(result.UserID, listID)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			if reference.IssueID == result.IssueID {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		if err := l.markAddResultTerminal(result.UserID, issue); err != nil {
			return nil, err
		}
		if err := l.store.RemoveIssue(issue.ID); err != nil {
			return nil, err
		}
		return replayIssueFromLedger(result), nil
	}

	latest, _, err := l.store.GetAddResult(result.IssueID)
	if err != nil {
		return nil, err
	}
	if err := validateAddResult(latest, candidate, result.UserID, result.ClientRequestID, result.PayloadSHA256); err != nil {
		return nil, err
	}
	if latest.State == addResultStateTerminal {
		if err := l.cleanupTerminalAdd(result.UserID, result.IssueID); err != nil {
			return nil, err
		}
		return replayIssueFromLedger(latest), nil
	}
	if latest.State != addResultStateCommitted {
		return nil, errors.Errorf("invalid add result state %q", latest.State)
	}

	issue.Replay = true
	return issue, nil
}

func (l *listManager) markAddResultTerminal(userID string, issue *Issue) error {
	if issue == nil || issue.ClientRequestID == "" {
		return nil
	}
	payloadSHA256 := addPayloadFingerprint(issue.Message, issue.PostPermalink, issue.Description, issue.PostID)

	for i := 0; i < StoreRetries; i++ {
		result, original, err := l.store.GetAddResult(issue.ID)
		if err != nil {
			return err
		}
		if result == nil {
			now := model.GetMillis()
			tombstone := &addResultLedger{
				Version:         addResultVersion,
				State:           addResultStateTerminal,
				UserID:          userID,
				ClientRequestID: issue.ClientRequestID,
				IssueID:         issue.ID,
				PayloadSHA256:   payloadSHA256,
				CreatedAt:       issue.CreateAt,
				UpdatedAt:       now,
				TerminalAt:      now,
			}
			inserted, err := l.store.SaveAddResultIfAbsent(tombstone)
			if err != nil {
				return err
			}
			if inserted {
				return nil
			}
			continue
		}
		expectedIssueID := newIdempotentIssue(userID, "", "", "", "", issue.ClientRequestID).ID
		if result.Version != addResultVersion ||
			result.UserID != userID ||
			result.ClientRequestID != issue.ClientRequestID ||
			result.IssueID != issue.ID ||
			result.IssueID != expectedIssueID ||
			result.PayloadSHA256 == "" {
			return errClientRequestIDConflict
		}
		if result.State == addResultStateTerminal {
			return nil
		}
		if result.State != addResultStateReserved && result.State != addResultStateCommitted {
			return errors.Errorf("invalid add result state %q", result.State)
		}

		terminal := *result
		terminal.State = addResultStateTerminal
		terminal.UpdatedAt = model.GetMillis()
		terminal.TerminalAt = terminal.UpdatedAt
		ok, err := l.store.CompareAndSetAddResult(original, &terminal)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}

	return errors.New("unable to persist terminal add result")
}

func (l *listManager) cleanupTerminalAdd(userID, issueID string) error {
	// Protocol-v1 self todos are not assignable (see ChangeAssignment), but an
	// interrupted pre-release build may have left an assigned correlated issue.
	// Cleanup may follow that link only when both sides prove the same exact
	// reciprocal tuple. Any conflicting or ambiguous topology stays intact for
	// operator repair instead of turning a terminal replay into a broad delete.
	type foreignCleanup struct {
		userID  string
		issueID string
		listID  string
	}

	var linkedUserID string
	var linkedIssueID string
	for _, listID := range []string{MyListKey, InListKey, OutListKey} {
		references, err := l.store.GetList(userID, listID)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if reference.IssueID != issueID {
				continue
			}
			if (reference.ForeignUserID == "") != (reference.ForeignIssueID == "") {
				return errors.New("ambiguous terminal add reference")
			}
			if reference.ForeignUserID == "" {
				continue
			}
			if linkedUserID != "" &&
				(linkedUserID != reference.ForeignUserID || linkedIssueID != reference.ForeignIssueID) {
				return errors.New("ambiguous terminal add foreign reference")
			}
			linkedUserID = reference.ForeignUserID
			linkedIssueID = reference.ForeignIssueID
		}
	}

	var foreign *foreignCleanup
	foreignLists := map[string]struct{}{}
	if linkedUserID != "" {
		_, foreignIssueExists, err := l.store.GetIssueIfExists(linkedIssueID)
		if err != nil {
			return err
		}

		for _, listID := range []string{MyListKey, InListKey, OutListKey} {
			references, err := l.store.GetList(linkedUserID, listID)
			if err != nil {
				return err
			}
			for _, reference := range references {
				if reference.IssueID != linkedIssueID {
					continue
				}
				if reference.ForeignUserID != userID || reference.ForeignIssueID != issueID {
					return errors.New("terminal add foreign reference is not reciprocal")
				}
				// Multiple exact copies are residue, not conflicting identity. The
				// complete scan above still rejects any distinct reciprocal tuple;
				// exact copies are removed together below.
				foreign = &foreignCleanup{userID: linkedUserID, issueID: linkedIssueID, listID: listID}
				foreignLists[listID] = struct{}{}
			}
		}

		if foreignIssueExists && foreign == nil {
			return errors.New("terminal add foreign issue has no reciprocal reference")
		}
		if foreignIssueExists {
			if err := l.store.RemoveIssue(linkedIssueID); err != nil {
				return err
			}
		}
		if foreign != nil {
			for listID := range foreignLists {
				if err := l.store.RemoveReferenceIfExists(foreign.userID, foreign.issueID, listID); err != nil {
					return err
				}
			}
		}
	}

	for _, listID := range []string{MyListKey, InListKey, OutListKey} {
		if err := l.store.RemoveReferenceIfExists(userID, issueID, listID); err != nil {
			return err
		}
	}
	return l.store.RemoveIssue(issueID)
}

func (l *listManager) SendIssue(senderID, receiverID, message, postPermalink, description, postID string) (string, error) {
	senderIssue := newIssue(message, postPermalink, description, postID)
	if err := l.store.SaveIssue(senderIssue); err != nil {
		return "", err
	}

	receiverIssue := newIssue(message, postPermalink, description, postID)
	if err := l.store.SaveIssue(receiverIssue); err != nil {
		if rollbackError := l.store.RemoveIssue(senderIssue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback sender issue after send error, Err=", err.Error())
		}
		return "", err
	}

	if err := l.store.AddReference(senderID, senderIssue.ID, OutListKey, receiverID, receiverIssue.ID); err != nil {
		if rollbackError := l.store.RemoveIssue(senderIssue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback sender issue after send error, Err=", err.Error())
		}
		if rollbackError := l.store.RemoveIssue(receiverIssue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback receiver issue after send error, Err=", err.Error())
		}
		return "", err
	}

	if err := l.store.AddReference(receiverID, receiverIssue.ID, InListKey, senderID, senderIssue.ID); err != nil {
		if rollbackError := l.store.RemoveIssue(senderIssue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback sender issue after send error, Err=", err.Error())
		}
		if rollbackError := l.store.RemoveIssue(receiverIssue.ID); rollbackError != nil {
			l.api.LogError("cannot rollback receiver issue after send error ,Err=", err.Error())
		}
		if rollbackError := l.store.RemoveReference(senderID, senderIssue.ID, OutListKey); rollbackError != nil {
			l.api.LogError("cannot rollback sender list after send error, Err=", err.Error())
		}
		return "", err
	}

	return receiverIssue.ID, nil
}

func (l *listManager) GetIssueList(userID, listID string) ([]*ExtendedIssue, error) {
	irs, err := l.store.GetList(userID, listID)
	if err != nil {
		return nil, err
	}

	extendedIssues := []*ExtendedIssue{}
	for _, ir := range irs {
		issue, err := l.store.GetIssue(ir.IssueID)
		if err != nil {
			continue
		}

		extendedIssue := l.extendIssueInfo(issue, ir)
		extendedIssues = append(extendedIssues, extendedIssue)
	}

	return extendedIssues, nil
}

func (l *listManager) GetAllList(userID string) (listsIssue *ListsIssue, err error) {
	inListIssue, err := l.GetIssueList(userID, InListKey)
	if err != nil {
		return nil, err
	}
	myListIssue, err := l.GetIssueList(userID, MyListKey)
	if err != nil {
		return nil, err
	}
	outListIssue, err := l.GetIssueList(userID, OutListKey)
	if err != nil {
		return nil, err
	}
	return &ListsIssue{
		In:  inListIssue,
		My:  myListIssue,
		Out: outListIssue,
	}, nil
}

func (l *listManager) CompleteIssue(userID, issueID string) (issue *Issue, foreignID string, listToUpdate string, err error) {
	claim, claimErr := l.resolveTerminalReferenceClaim(userID, issueID)
	if claimErr != nil {
		return nil, "", "", claimErr
	}
	issueList, ir := claim.listID, claim.reference
	issue, err = l.store.GetIssue(issueID)
	if err != nil {
		return nil, "", issueList, err
	}

	// Strict CAS removal is the user action claim: concurrent terminal requests
	// must have exactly one winner. Replay cleanup remains idempotent elsewhere.
	if err = l.consumeTerminalReferenceClaim(userID, issueID, claim); err != nil {
		return nil, "", issueList, err
	}
	// A crash between the claim and this tombstone cannot resurrect the add:
	// committed replay treats a missing owner reference as terminal and never
	// recreates it.
	if err = l.markAddResultTerminal(userID, issue); err != nil {
		return nil, "", issueList, err
	}

	if removeErr := l.store.RemoveIssue(issueID); removeErr != nil {
		l.api.LogError("cannot remove issue, Err=", removeErr.Error())
	}

	if ir.ForeignUserID == "" {
		return issue, "", issueList, nil
	}

	err = l.store.RemoveReference(ir.ForeignUserID, ir.ForeignIssueID, OutListKey)
	if err != nil {
		l.api.LogError("cannot clean foreigner list after complete, Err=", err.Error())
	}

	issue, err = l.store.GetAndRemoveIssue(ir.ForeignIssueID)
	if err != nil {
		l.api.LogError("cannot clean foreigner issue after complete, Err=", err.Error())
	}

	return issue, ir.ForeignUserID, issueList, nil
}

func (l *listManager) EditIssue(userID, issueID, newMessage, newDescription string) (foreignUserID, list, oldMessage string, err error) {
	issue, err := l.store.GetIssue(issueID)
	if err != nil {
		return "", "", "", err
	}

	list, ir, _ := l.store.GetIssueListAndReference(userID, issueID)
	if ir == nil {
		return "", "", "", errors.New("reference not found")
	}

	if ir.ForeignIssueID != "" {
		foreignIssue, foreignErr := l.store.GetIssue(ir.ForeignIssueID)
		if foreignErr == nil {
			oldMessage = foreignIssue.Message
			foreignIssue.Message = newMessage
			foreignIssue.Description = newDescription
			foreignErr = l.store.SaveIssue(foreignIssue)
			if foreignErr != nil {
				l.api.LogError("cannot edit foreign issue after edit", "error", foreignErr.Error())
			}
		}
	}

	issue.Message = newMessage
	issue.Description = newDescription
	err = l.store.SaveIssue(issue)
	if err != nil {
		return "", "", "", err
	}

	return ir.ForeignUserID, list, oldMessage, nil
}

func (l *listManager) ChangeAssignment(issueID string, userID string, sendTo string) (issue *Issue, oldOwner string, receiverIssueID string, err error) {
	issue, err = l.store.GetIssue(issueID)
	if err != nil {
		return nil, "", "", err
	}
	list, ir, _ := l.store.GetIssueListAndReference(userID, issueID)
	if ir == nil {
		return nil, "", "", errors.New("reference not found")
	}

	if list == InListKey {
		return nil, "", "", errors.New("trying to change the assignment of a todo not owned")
	}
	// A protocol-v1 add result is actor-scoped and its committed replay scans
	// that actor's lists. ChangeAssignment is a multi-key move with a gap between
	// removing My and adding Out/In; a concurrent replay could otherwise mistake
	// that gap for a terminal delete and erase the live issue. Keep the correlated
	// boundary fail-closed until assignment has its own atomic identity protocol.
	// The ownership/reference checks above intentionally precede this guard so a
	// guessed foreign issue ID does not disclose its correlation state.
	if issue.ClientRequestID != "" {
		return nil, "", "", errCorrelatedIssueAssignmentUnsupported
	}

	if ir.ForeignUserID != "" {
		// Remove reference from foreign user
		foreignList, foreignIR, _ := l.store.GetIssueListAndReference(ir.ForeignUserID, ir.ForeignIssueID)
		if foreignIR == nil {
			return nil, "", "", errors.New("reference not found")
		}

		if err := l.store.RemoveReference(ir.ForeignUserID, ir.ForeignIssueID, foreignList); err != nil {
			return nil, "", "", err
		}

		_, err := l.store.GetAndRemoveIssue(ir.ForeignIssueID)
		if err != nil {
			l.api.LogError("cannot remove issue", "err", err.Error())
		}
	}

	if userID == sendTo && list == OutListKey {
		if err := l.store.RemoveReference(userID, issueID, OutListKey); err != nil {
			return nil, "", "", err
		}

		if err := l.store.AddReference(userID, issueID, MyListKey, "", ""); err != nil {
			return nil, "", "", err
		}

		return issue, ir.ForeignUserID, issueID, nil
	}

	if userID != sendTo {
		if err := l.store.RemoveReference(userID, issueID, list); err != nil {
			return nil, "", "", err
		}
	}

	receiverIssue := newIssue(issue.Message, issue.PostPermalink, issue.Description, issue.PostID)
	if err := l.store.SaveIssue(receiverIssue); err != nil {
		return nil, "", "", err
	}

	if err := l.store.AddReference(userID, issueID, OutListKey, sendTo, receiverIssue.ID); err != nil {
		return nil, "", "", err
	}

	if err := l.store.AddReference(sendTo, receiverIssue.ID, InListKey, userID, issue.ID); err != nil {
		return nil, "", "", err
	}

	return issue, ir.ForeignUserID, receiverIssue.ID, nil
}

func (l *listManager) AcceptIssue(userID, issueID string) (todoMessage string, foreignUserID string, outErr error) {
	issue, err := l.store.GetIssue(issueID)
	if err != nil {
		return "", "", err
	}

	ir, _, err := l.store.GetIssueReference(userID, issueID, InListKey)
	if err != nil {
		return "", "", err
	}
	if ir == nil {
		return "", "", fmt.Errorf("element reference not found")
	}

	err = l.store.AddReference(userID, issueID, MyListKey, ir.ForeignUserID, ir.ForeignIssueID)
	if err != nil {
		return "", "", err
	}

	err = l.store.RemoveReference(userID, issueID, InListKey)
	if err != nil {
		if rollbackError := l.store.RemoveReference(userID, issueID, MyListKey); rollbackError != nil {
			l.api.LogError("cannot rollback accept operation, Err=", rollbackError.Error())
		}
		return "", "", err
	}

	if issue.PostPermalink != "" {
		issue.Message = fmt.Sprintf("%s\n[Permalink](%s)", issue.Message, issue.PostPermalink)
	}

	return issue.Message, ir.ForeignUserID, nil
}

func (l *listManager) RemoveIssue(userID, issueID string) (outIssue *Issue, foreignID string, isSender bool, listToUpdate string, outErr error) {
	claim, claimErr := l.resolveTerminalReferenceClaim(userID, issueID)
	if claimErr != nil {
		return nil, "", false, "", claimErr
	}
	issueList, ir := claim.listID, claim.reference
	issue, err := l.store.GetIssue(issueID)
	if err != nil {
		return nil, "", false, issueList, err
	}

	// This strict CAS is the terminal-action claim. Only its winner may continue
	// into handler-visible side effects.
	if err := l.consumeTerminalReferenceClaim(userID, issueID, claim); err != nil {
		return nil, "", false, issueList, err
	}
	if err := l.markAddResultTerminal(userID, issue); err != nil {
		return nil, "", false, issueList, err
	}

	if removeErr := l.store.RemoveIssue(issueID); removeErr != nil {
		l.api.LogError("cannot remove issue, Err=", removeErr.Error())
	}

	if ir.ForeignUserID == "" {
		return issue, "", false, issueList, nil
	}

	list, _, _ := l.store.GetIssueListAndReference(ir.ForeignUserID, ir.ForeignIssueID)

	err = l.store.RemoveReference(ir.ForeignUserID, ir.ForeignIssueID, list)
	if err != nil {
		l.api.LogError("cannot clean foreigner list after remove, Err=", err.Error())
	}

	issue, err = l.store.GetAndRemoveIssue(ir.ForeignIssueID)
	if err != nil {
		l.api.LogError("cannot clean foreigner issue after remove, Err=", err.Error())
	}

	return issue, ir.ForeignUserID, list == OutListKey, issueList, nil
}

func (l *listManager) PopIssue(userID string) (issue *Issue, foreignID string, err error) {
	myReferences, err := l.store.GetList(userID, MyListKey)
	if err != nil || len(myReferences) == 0 {
		if err != nil {
			return nil, "", err
		}
		return nil, "", errors.New("cannot find issue")
	}
	targetIssueID := myReferences[0].IssueID
	claim, err := l.resolveTerminalReferenceClaim(userID, targetIssueID)
	if err != nil {
		return nil, "", err
	}

	var ir *IssueRef
	if claim.crossList {
		// The actor+issue claim is the sole winner across every owner list.
		if err := l.consumeTerminalReferenceClaim(userID, targetIssueID, claim); err != nil {
			return nil, "", err
		}
		ir = claim.reference
	} else {
		// PopReference pins its own observed head across CAS retries. Keeping the
		// actual pop here preserves FIFO if a normal list changed after the peek.
		ir, err = l.store.PopReference(userID, MyListKey, targetIssueID)
		if err != nil {
			return nil, "", err
		}
	}

	issue, err = l.store.GetIssue(ir.IssueID)
	if err != nil {
		return nil, "", err
	}
	if err = l.markAddResultTerminal(userID, issue); err != nil {
		return nil, "", err
	}
	if removeErr := l.store.RemoveIssue(ir.IssueID); removeErr != nil {
		l.api.LogError("cannot remove issue after pop, Err=", removeErr.Error())
	}

	if ir.ForeignUserID == "" {
		return issue, "", nil
	}

	err = l.store.RemoveReference(ir.ForeignUserID, ir.ForeignIssueID, OutListKey)
	if err != nil {
		l.api.LogError("cannot clean foreigner list after pop, Err=", err.Error())
	}
	issue, err = l.store.GetAndRemoveIssue(ir.ForeignIssueID)
	if err != nil {
		l.api.LogError("cannot clean foreigner issue after pop, Err=", err.Error())
	}

	return issue, ir.ForeignUserID, nil
}

func (l *listManager) BumpIssue(userID, issueID string) (todo *Issue, receiver string, foreignIssueID string, outErr error) {
	ir, _, err := l.store.GetIssueReference(userID, issueID, OutListKey)
	if err != nil {
		return nil, "", "", err
	}

	if ir == nil {
		return nil, "", "", fmt.Errorf("cannot find sender issue")
	}

	err = l.store.BumpReference(ir.ForeignUserID, ir.ForeignIssueID, InListKey)
	if err != nil {
		return nil, "", "", err
	}

	issue, err := l.store.GetIssue(ir.ForeignIssueID)
	if err != nil {
		l.api.LogError("cannot find foreigner issue after bump, Err=", err.Error())
		return nil, "", "", nil
	}

	return issue, ir.ForeignUserID, ir.ForeignIssueID, nil
}

func (l *listManager) GetUserName(userID string) string {
	user, err := l.api.GetUser(userID)
	if err != nil {
		return "Someone"
	}
	return user.Username
}

func (l *listManager) extendIssueInfo(issue *Issue, ir *IssueRef) *ExtendedIssue {
	if issue == nil || ir == nil {
		return nil
	}

	feIssue := &ExtendedIssue{
		Issue: *issue,
	}

	if ir.ForeignUserID == "" {
		return feIssue
	}

	list, _, n := l.store.GetIssueListAndReference(ir.ForeignUserID, ir.ForeignIssueID)

	var listName string
	switch list {
	case MyListKey:
		listName = MyListKey
	case InListKey:
		listName = InFlag
	case OutListKey:
		listName = OutFlag
	}

	userName := l.GetUserName(ir.ForeignUserID)

	feIssue.ForeignUser = userName
	feIssue.ForeignList = listName
	feIssue.ForeignPosition = n

	return feIssue
}
