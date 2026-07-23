package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi/experimental/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestServeHTTP(t *testing.T) {
	assert.True(t, true)
}

type editListManagerStub struct {
	ListManager

	foreignUserID string
	list          string
	calls         []editCall
}

type editCall struct {
	userID      string
	issueID     string
	message     string
	description string
}

func (s *editListManagerStub) EditIssue(userID, issueID, message, description string) (string, string, string, error) {
	s.calls = append(s.calls, editCall{
		userID:      userID,
		issueID:     issueID,
		message:     message,
		description: description,
	})

	return s.foreignUserID, s.list, "old title", nil
}

func (s *editListManagerStub) GetUserName(string) string {
	return "actor"
}

type noopTelemetryTracker struct{}

func (noopTelemetryTracker) TrackEvent(string, map[string]interface{}) error {
	return nil
}

func (noopTelemetryTracker) TrackUserEvent(string, string, map[string]interface{}) error {
	return nil
}

func (noopTelemetryTracker) ReloadConfig(telemetry.TrackerConfig) {}

func TestHandleEditRefreshesMirroredTodosWithoutBotDM(t *testing.T) {
	testCases := []struct {
		name            string
		actorID         string
		foreignUserID   string
		list            string
		message         string
		description     string
		repeat          int
		foreignLists    []string
		expectForeignWS bool
	}{
		{
			name:            "sender title edit",
			actorID:         "sender",
			foreignUserID:   "receiver",
			list:            OutListKey,
			message:         "updated title",
			description:     "description",
			repeat:          1,
			foreignLists:    []string{MyListKey, InListKey},
			expectForeignWS: true,
		},
		{
			name:            "receiver description edit",
			actorID:         "receiver",
			foreignUserID:   "sender",
			list:            InListKey,
			message:         "same title",
			description:     "updated description",
			repeat:          1,
			foreignLists:    []string{OutListKey},
			expectForeignWS: true,
		},
		{
			name:            "repeated retry edit",
			actorID:         "sender",
			foreignUserID:   "receiver",
			list:            OutListKey,
			message:         "same title",
			description:     "same description",
			repeat:          3,
			foreignLists:    []string{MyListKey, InListKey},
			expectForeignWS: true,
		},
		{
			name:            "self todo edit",
			actorID:         "owner",
			foreignUserID:   "",
			list:            MyListKey,
			message:         "updated self todo",
			description:     "description",
			repeat:          1,
			expectForeignWS: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			listManager := &editListManagerStub{
				foreignUserID: tc.foreignUserID,
				list:          tc.list,
			}
			plugin := &Plugin{
				listManager: listManager,
				tracker:     noopTelemetryTracker{},
			}
			plugin.SetAPI(api)

			api.On(
				"PublishWebSocketEvent",
				WSEventRefresh,
				map[string]interface{}{"lists": []string{tc.list}},
				mock.MatchedBy(func(broadcast *model.WebsocketBroadcast) bool {
					return broadcast != nil && broadcast.UserId == tc.actorID
				}),
			).Return().Times(tc.repeat)

			if tc.expectForeignWS {
				api.On(
					"PublishWebSocketEvent",
					WSEventRefresh,
					map[string]interface{}{"lists": tc.foreignLists},
					mock.MatchedBy(func(broadcast *model.WebsocketBroadcast) bool {
						return broadcast != nil && broadcast.UserId == tc.foreignUserID
					}),
				).Return().Times(tc.repeat)
			}

			for range tc.repeat {
				request := httptest.NewRequest(
					http.MethodPut,
					"/edit",
					strings.NewReader(`{"id":"todo-1","message":"`+tc.message+`","description":"`+tc.description+`"}`),
				)
				request.Header.Set("Mattermost-User-ID", tc.actorID)
				response := httptest.NewRecorder()

				plugin.handleEdit(response, request)

				assert.Equal(t, http.StatusOK, response.Code)
			}

			require.Len(t, listManager.calls, tc.repeat)
			for _, call := range listManager.calls {
				assert.Equal(t, editCall{
					userID:      tc.actorID,
					issueID:     "todo-1",
					message:     tc.message,
					description: tc.description,
				}, call)
			}

			api.AssertExpectations(t)
			api.AssertNotCalled(t, "GetDirectChannel", mock.Anything, mock.Anything)
			api.AssertNotCalled(t, "CreatePost", mock.Anything)
		})
	}
}
