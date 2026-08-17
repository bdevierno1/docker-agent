package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

func TestAgentSourceErrorsAreTyped(t *testing.T) {
	t.Parallel()

	sm := NewSessionManager(t.Context(), config.Sources{
		"broken": &mockSource{name: "broken", err: errors.New("unavailable")},
	}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	_, err := sm.LoadAgentConfig(t.Context(), "missing")
	require.ErrorIs(t, err, ErrAgentNotFound)
	_, err = sm.LoadAgentConfig(t.Context(), "broken")
	require.ErrorIs(t, err, ErrAgentSourceUnavailable)
	_, err = sm.GetAgentToolCount(t.Context(), "broken", "")
	require.ErrorIs(t, err, ErrAgentSourceUnavailable)
}

func TestAgentSourceHTTPStatus(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	sm := NewSessionManager(t.Context(), config.Sources{
		"broken": &mockSource{name: "broken", err: errors.New("unavailable")},
	}, store, 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "")

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"config missing", http.MethodGet, "/api/agents/missing", http.StatusNotFound},
		{"config unavailable", http.MethodGet, "/api/agents/broken", http.StatusBadGateway},
		{"tool count missing", http.MethodGet, "/api/agents/missing/root/tools/count", http.StatusNotFound},
		{"tool count unavailable", http.MethodGet, "/api/agents/broken/root/tools/count", http.StatusBadGateway},
		{"run missing", http.MethodPost, "/api/sessions/" + sess.ID + "/agent/missing", http.StatusNotFound},
		{"run unavailable", http.MethodPost, "/api/sessions/" + sess.ID + "/agent/broken", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, bytes.NewBufferString(`{"messages":[]}`))
			rec := httptest.NewRecorder()
			srv.e.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestGetAgentsSkipsUnavailableSources(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	srv := NewWithManager(NewSessionManager(t.Context(), config.Sources{
		"broken":  &mockSource{name: "broken", err: errors.New("unavailable")},
		"healthy": config.NewBytesSource("healthy", []byte("version: \"2\"\nagents:\n  root:\n    instruction: hi\n    model: openai/gpt-4o\n")),
	}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}), "")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/agents", nil)
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"healthy"`)
	assert.NotContains(t, rec.Body.String(), "broken")
}
