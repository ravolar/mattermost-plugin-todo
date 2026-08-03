package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// Issue represents a Todo issue
type Issue struct {
	ID              string `json:"id"`
	Message         string `json:"message"`
	PostPermalink   string `json:"postPermalink"`
	Description     string `json:"description,omitempty"`
	CreateAt        int64  `json:"create_at"`
	PostID          string `json:"post_id"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	Replay          bool   `json:"-"`
	TerminalReplay  bool   `json:"-"`
}

func addPayloadFingerprint(message, postPermalink, description, postID string) string {
	payload, err := json.Marshal(struct {
		Message       string `json:"message"`
		PostPermalink string `json:"post_permalink"`
		Description   string `json:"description"`
		PostID        string `json:"post_id"`
	}{
		Message:       message,
		PostPermalink: postPermalink,
		Description:   description,
		PostID:        postID,
	})
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// ExtendedIssue extends the information on Issue to be used on the front-end
type ExtendedIssue struct {
	Issue
	ForeignUser     string `json:"user"`
	ForeignList     string `json:"list"`
	ForeignPosition int    `json:"position"`
}

// ListsIssue for all list issues
type ListsIssue struct {
	In  []*ExtendedIssue `json:"in"`
	My  []*ExtendedIssue `json:"my"`
	Out []*ExtendedIssue `json:"out"`
}

func newIssue(message, postPermalink, description, postID string) *Issue {
	return &Issue{
		ID:            model.NewId(),
		CreateAt:      model.GetMillis(),
		Message:       message,
		PostPermalink: postPermalink,
		Description:   description,
		PostID:        postID,
	}
}

// newIdempotentIssue derives a stable Mattermost-compatible issue ID from the
// actor and client request ID. The actor is part of the digest so two users may
// safely use the same request ID. The 104-bit truncated digest preserves the
// plugin's existing 26-character ID length and alphabet constraints.
func newIdempotentIssue(userID, message, postPermalink, description, postID, clientRequestID string) *Issue {
	digest := sha256.Sum256([]byte("todo-add-v1\x00" + userID + "\x00" + clientRequestID))

	return &Issue{
		ID:              hex.EncodeToString(digest[:])[:26],
		CreateAt:        model.GetMillis(),
		Message:         message,
		PostPermalink:   postPermalink,
		Description:     description,
		PostID:          postID,
		ClientRequestID: clientRequestID,
	}
}

func issuesListToString(issues []*ExtendedIssue) string {
	if len(issues) == 0 {
		return "Nothing to do!"
	}

	str := "\n\n"

	for _, issue := range issues {
		createAt := time.Unix(issue.CreateAt/1000, 0)
		str += fmt.Sprintf("* %s\n  * (%s)\n", issue.Message, createAt.Format("January 2, 2006 at 15:04"))
	}

	return str
}
